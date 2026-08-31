# Implementation Plan: VM-Owned Volume Brownfield Migration

- **Spec**: [`spec.md`](./spec.md)
- **Epic**: vmop-TBD
- **Date**: 2026-08-07
- **Full external spec**: `cns-specs/VGL-62908/vm-owned-volume-migration-spec.md`
- **Implementation sequencing**: `cns-specs/VGL-62908/implementation/vmop.md` §15–16 (V11, V12)

---

## Summary

Add a migration reconcile path to the existing volume attach/detach controller, invoked before any VM-owned or legacy workflow, that converts a brownfield VM's already-attached disks onto the CsiVolumeInfo path in place and flips the VM annotation once complete. No new controller, no new CRD.

---

## Technical context

| Field | Value |
|-------|-------|
| **Modules touched** | Root module only — no external-type or CRD change (D1 still holds; migration adds annotations, not fields) |
| **API version(s) touched** | `vmoperator.vmware.com/v1alpha6` (`VirtualMachine` — two existing annotation constants); `cns.vmware.com/v1alpha1` (`CnsNodeVMBatchAttachment`, `CsiVolumeInfo` — both already-mirrored external types, annotations only) |
| **New dependencies** | None |

---

## Constitution check

| Rule | Status | Notes |
|------|--------|-------|
| No new CR, no new CVI status field (migration spec §6) | OK | Migration adds only the `vmoperator.vmware.com/migrate-to-vm-owned` and `cns.vmware.com/vm-owned-migration` annotations, both already reserved as constants from [002](../002-vm-owned-volumes/)'s commit 1. |
| Thin controllers — business logic in `pkg/` where the code is genuinely reusable | Deviation, justified | The migration orchestration lives directly in `controllers/virtualmachine/volumeattachdetach/volumeattachdetach_migration.go` rather than a `pkg/` package. See Complexity tracking. |
| No controller calls vSphere APIs directly | OK | The one device operation (VKS disk-mode conversion) goes through a new `providers.VirtualMachineProviderInterface` method (`ConvertDiskToIndependentPersistent`), matching the existing `AttachVolumeDisks`/`DetachDiskAtSlot` pattern. |
| Controllers don't write `spec` | Deviation, justified (spec-mandated) | The VKS conversion rewrites `vm.spec.volumes[*].diskMode` directly — the same category of exception `restoreVMSpecFromSnapshot` already takes, called out inline where it happens. |
| Level-triggered idempotency, no in-flight state markers | OK | No new phase enum. Migration's own progress is inferred from the BA's annotation, the CVI's `spec.vms`/`status.ownership`, and `vm.spec.volumes[*].diskMode` vs. the live device — never a dedicated "migrating" field. |
| Field-indexed, bounded list scans | OK | Reuses the existing `CVIVMInstanceUUIDIndexKey`/`CVIVMNameIndexKey` indexes and the existing per-VM BA lookup by name; no new scan pattern. |
| One test file per package, `testlabels` | OK | New test file follows the existing package's layout (`volumeattachdetach_migration_unit_test.go`, `testlabels.Controller`). |

---

## Project structure

```
controllers/virtualmachine/volumeattachdetach/
├── volumeattachdetach_controller.go        MODIFY — ReconcileNormal gains a migration-candidate
│                                                    branch ahead of the existing VM-owned/legacy
│                                                    branches; returns early (requeuing) until
│                                                    migration completes
├── volumeattachdetach_migration.go         NEW — isMigrationCandidate, reconcileMigration,
│                                                    completeMigration, convertVKSDiskModes,
│                                                    cnsDiskModeToCVIDiskMode
└── volumeattachdetach_migration_unit_test.go   NEW — fake-client unit tests

pkg/providers/
├── vm_provider_interface.go                MODIFY — HasAnySnapshot, ConvertDiskToIndependentPersistent
└── fake/fake_vm_provider.go                MODIFY — matching Fn overrides

pkg/providers/vsphere/
└── vmprovider_vm_ownedvolumes_migration.go NEW — HasAnySnapshot (moVM.Snapshot presence check),
                                                    ConvertDiskToIndependentPersistent (device edit,
                                                    reuses findVirtualDiskDeviceAtSlot, diskModeToVim,
                                                    assertNoVMLevelCBT from the attach/detach commit)
```

No RBAC change: `cnsnodevmbatchattachments` already carries every verb this needs (create/delete/get/list/patch/update/watch, from [002](../002-vm-owned-volumes/)'s V13 audit, which specifically confirmed this ahead of time), `csivolumeinfos` already has `create`, and the aggregated `manager-role` already grants `patch`/`update` on `virtualmachines` (contributed by other controllers' markers) for the annotation and spec writes.

---

## API / CRD strategy

No schema change. Two annotation constants — `pkgconst.MigrateToVMOwnedAnnotation` and `pkgconst.VMOwnedMigrationAnnotation` (with `VMOwnedMigrationInProgress`/`VMOwnedMigrationComplete` values) — were added to `pkg/constants/constants.go` ahead of time, in [002](../002-vm-owned-volumes/)'s commit 1, in anticipation of this spec.

---

## Controller / webhook impact

### Volume controller (`controllers/virtualmachine/volumeattachdetach/`)

`ReconcileNormal` gains a check, ahead of the existing VM-owned and legacy branches, once the VM's `InstanceUUID`/`BiosUUID` are known:

```
if VMOwnedVolumes enabled:
    if isMigrationCandidate(vm):     // gate on, annotation absent, PVC volume or explicit trigger
        return reconcileMigration(ctx)   // requeues until complete
    if HasVMOwnedVolumesAnnotation(vm):
        reconcileOwnedVolumes(ctx)
// ... existing legacy/batch path unchanged
```

`reconcileMigration`:

1. Looks up the VM's `CnsNodeVMBatchAttachment` (nil is valid — a VM with no prior attach).
2. **Stage 1 — freeze**: patches `cns.vmware.com/vm-owned-migration: InProgress` onto the BA if not already present.
3. **VKS conversion** (`convertVKSDiskModes`), if `kubeutil.HasCAPILabels(vm.Labels)`: prechecks `HasAnySnapshot`, then per non-boot disk (every disk on the BA, by construction) rewrites `vm.spec.volumes[*].diskMode`, reconfigures the device via `ConvertDiskToIndependentPersistent`, and updates the in-memory BA volume spec so the next step sees the new mode without a re-fetch.
4. **Per-disk loop**: for each `BA.Spec.Volumes` entry, resolves/creates the CVI (`vmopv1util.EnsureCVIForPVC`), appends or updates this VM's entry (mode converted from the BA's own `DiskMode` type via `cnsDiskModeToCVIDiskMode` — the BA's `VolumeSpec` is the authoritative record of a disk's current mode, so this never needs to consult `vm.spec.volumes`, which may already have moved on if the triggering edit was a detach), re-reads the CVI to confirm the entry landed, then removes the volume from `BA.Spec.Volumes`. Tracks per-disk readiness: a dependent entry needs `status.ownership == VMManaged` (clean or `fcd-retained`, both count); an independent entry is ready as soon as its entry is confirmed.
5. Applies the accumulated `BA.Spec.Volumes` removal as one patch.
6. If every disk is ready, calls `completeMigration`; otherwise returns `pkgerr.RequeueError`.

`completeMigration` patches the VM's `vm-owned-volumes` annotation directly (not via the outer `Reconcile`'s deferred patch — that would let the BA delete below race ahead of it on a crash), then, if a BA exists, patches its migration annotation to `Complete` and issues `Delete`.

### Provider (`pkg/providers/vsphere/`)

`ConvertDiskToIndependentPersistent` is a device **edit** (`VirtualDeviceConfigSpecOperationEdit`), not an add — no `vDiskId`, no CBT directive, but still guarded by `assertNoVMLevelCBT` since it is a `ReconfigVM_Task` against a VM that may carry retained FCDs. `HasAnySnapshot` is a thin `moVM.Snapshot` presence check, reusing the same properties-collector pattern as `IsDiskRetainedByAnySnapshot`.

---

## Test strategy

| Layer | Mechanism | Location |
|-------|-----------|----------|
| Unit (Ginkgo, fake client) | `testlabels.Controller` | `controllers/virtualmachine/volumeattachdetach/volumeattachdetach_migration_unit_test.go` — freeze+entry+BA-removal+requeue, completion on `VMManaged`, `fcd-retained` counts as migrated, independent re-home completes in one pass, explicit trigger with no BA, already-annotated VM is not a candidate, VKS conversion (device call args, spec rewrite, CVI mode) and its snapshot-precheck stall |
| Integration / E2E | — | Deferred with the same rationale as [002](../002-vm-owned-volumes/): a full pass belongs once independent-mode device attach (that spec's tracked gap) and this migration path are both exercised together against a real or simulated CSI. |

---

## Rollout / migration

- No feature-flag change: reuses `pkgcfg.Features.VMOwnedVolumes`.
- Not reversible: a VM that completes migration cannot be moved back to brownfield by this code; Appendix A of the migration spec sketches an optional future rollback, not implemented here.
- Depends on CSI implementing the migration spec's §8–§13 (deferred-unregister branch, auto-complete watch, skip-register on detach) for the deferred and convergence paths to behave as specified; vm-operator's orchestration is correct independent of CSI's rollout state (it degrades to "always waiting" if CSI never flips a CVI to `VMManaged`, which is the same safe failure mode as attach/detach's own dependence on CSI).

---

## Complexity tracking

| Item | Why needed | Simpler alternative rejected because |
|------|------------|--------------------------------------|
| Migration orchestration lives in `controllers/` rather than `pkg/volumes/owned/` | The whole flow is a single, non-reused sequence tightly coupled to this one controller's BA/CVI/VM objects and its own patch helper usage; splitting it into a `pkg/` package today would add an indirection with no second caller. | `pkg/volumes/owned/` was reserved for genuinely reusable classification/decision logic invoked from more than one place ([002](../002-vm-owned-volumes/)'s `classify.go`/`decision.go`); forcing this in would be a premature abstraction. |
| `convertVKSDiskModes` writes `vm.spec.volumes[*].diskMode` directly from a controller | Migration spec §4.5 mandates the rewrite as part of conversion, and nothing else ever writes that field on a node VM. | Routing the write through a webhook or a separate reconciler would split one atomic-in-intent operation (rewrite mode → reconfigure device → append CVI entry, in that order) across two reconcile loops with no way to guarantee the ordering. |
| `cnsDiskModeToCVIDiskMode` bridges two independently-named `DiskMode` types | The BA's `VolumeSpec.PersistentVolumeClaim.DiskMode` (lower-snake-case, this module's mirror of CSI's BA type) and the CVI's `CVIDiskMode` (PascalCase) are different Go types from different mirrored packages; migration is the one place that needs to read the former and write the latter for a volume that may have already left `vm.spec.volumes`. | Resolving the mode from `vm.spec.volumes` instead was considered and rejected: for the very detach that triggers migration, the volume may already be gone from `spec.volumes` by the time migration reconciles, leaving the BA's own record as the only source of truth for "what mode is this disk attached in right now." |
