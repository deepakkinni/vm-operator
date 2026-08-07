# Implementation Plan: VM-Owned Volume Attach/Detach

- **Spec**: [`spec.md`](./spec.md)
- **Epic**: vmop-TBD
- **Date**: 2026-06-23
- **Updated**: 2026-08-07
- **Full external spec**: `cns-specs/VGL-62908/vm-owned-volume-attach-detach-spec.md`
- **Implementation sequencing**: `cns-specs/VGL-62908/implementation/vmop.md` (this plan mirrors its §1–§14, §17 — Pass 1; migration, §15–16, is Pass 2 and out of scope here)

---

## Summary

Route every PVC-backed disk on a VM-owned-volumes VM — in every disk mode, not only dependent — through the `CsiVolumeInfo` (CVI) CR instead of `CnsNodeVMBatchAttachment` (BA), and attach/detach the disk directly via `ReconfigVM_Task`. All behavior is gated behind the `VMOwnedVolumes` feature gate and the per-VM `vmoperator.vmware.com/vm-owned-volumes` annotation; a VM lacking either continues through the existing legacy path unchanged.

---

## Technical context

| Field | Value |
|-------|-------|
| **Language** | Go 1.23+ |
| **Primary dependencies** | `controller-runtime`, `govmomi`, `kubebuilder` |
| **API server** | Kubernetes (vSphere Supervisor) |
| **Testing** | Ginkgo v2 + Gomega; envtest for controller/webhook integration; plain-Go unit tests for provider-layer vim25 device logic |
| **Code generation** | `controller-gen` (deepcopy, CRD manifests, RBAC markers) — note: this codebase's `object` (deepcopy) generator panics on unrelated pre-existing types; the `crd` and `rbac` generators are unaffected and were used directly |
| **Target platform** | VMware vSphere Supervisor (WCP); namespace-isolated multi-tenancy |
| **API version(s) touched** | `vmoperator.vmware.com/v1alpha6` (`VirtualMachine` — annotation only, no field added); external `cns.vmware.com/v1alpha1` (`CsiVolumeInfo` — mirrors CSI's authoritative type) |
| **Modules touched** | Root module (`github.com/vmware-tanzu/vm-operator`); `external/vsphere-csi-driver/` sub-module (its own `go.mod`, wired via a `replace` directive and vendored) |
| **New dependencies** | None |

---

## Constitution check

| Rule | Status | Notes |
|------|--------|-------|
| API compatibility — additive only, no version bump | OK | Only an existing annotation constant and constants for the CVI mirror type are added. No `VirtualMachine` field is added, removed, or renamed (D1). |
| No vm-operator CRD changes | OK | D1: all new state lives on the CVI, which vm-operator does not own — it only reads and patches `spec.vms`/`spec.diskPath`, mirroring CSI's authoritative type byte-for-byte in `external/vsphere-csi-driver/api/v1alpha1/`. |
| Thin controllers — business logic in `pkg/` | OK | `controllers/virtualmachine/volumeattachdetach/volumeattachdetach_ownedvolumes.go` orchestrates; classification (`pkg/volumes/owned/classify.go`), decision-making (`pkg/volumes/owned/decision.go`), and mode/CVI helpers (`pkg/util/vmopv1/vmowned_volumes*.go`) hold the logic. |
| No controller calls vSphere APIs directly | OK | All `ReconfigVM_Task` calls go through `pkg/providers/vsphere/vmprovider_vm_ownedvolumes.go` via the `providers.VMProviderInterface` (`AttachVolumeDisks`, `GetLiveDiskPathAtSlot`, `DetachDiskAtSlot`). |
| Controllers track `status.observedGeneration` and readiness | OK | CVI status is owned by CSI; vm-operator reads the green signal (`vmopv1util.IsGreenSignal`) but never writes CVI `status`. |
| Field-indexed, bounded list scans (no unbounded scans at scale) | OK | `CVIVMInstanceUUIDIndexKey`/`CVIVMNameIndexKey` field indexes back every CVI lookup by VM; the sweeper lists only CVIs, never VMs cluster-wide. |
| Level-triggered idempotency, no in-flight state markers | OK | No `Detaching`/`Attaching` phase anywhere; state is always inferred fresh from live disk presence, CVI entries, `vm.spec.volumes`, and the snapshot tree. |
| Webhooks in `webhooks/`; Go validation for CVI-dependent checks | OK | `validateOwnedVolumeAttach` (VM validator) and `isVMOwnedPVCDeleteDenied` (PVC validator) both need a CVI lookup, so they are Go, not CEL. |
| Controllers for non-`vmoperator.vmware.com` API groups outside `controllers/vmoperator.vmware.com`-scoped packages | OK | `controllers/csivolumeinfo/` is its own top-level package — it sweeps a `cns.vmware.com` type, not a vm-operator type. |
| One test file per package, one suite bootstrap, labels from `testlabels` | OK | New test files follow the standard layout; new packages (`controllers/csivolumeinfo/`, `pkg/volumes/owned/`) each got their own suite bootstrap. |
| No internal Broadcom URLs in tracked files | OK | Tickets referenced as `vmop-NNN`. |

---

## Project structure

Reflects what actually shipped across commits 1–9 (Pass 1, V1–V10) plus this commit's wiring (V13). Migration (V11–V12) is a separate follow-on spec.

```
external/vsphere-csi-driver/
└── api/v1alpha1/
    ├── csivolumeinfo_types.go        MODIFY — CsiVolumeInfo mirrors CSI's authoritative type
    │                                          byte-for-byte, incl. spec.vms[*].volumeName (D2)
    └── zz_generated.deepcopy.go      MODIFY — hand-fixed rename (object generator panics here)

config/crd/external-crds/
└── cns.vmware.com_csivolumeinfos.yaml   NEW — generated via controller-gen crd (was missing
                                                entirely before commit 1; no envtest had ever
                                                installed it)

pkg/constants/constants.go             MODIFY — VMOwnedVolumesAnnotation (pre-existing),
                                                MigrateToVMOwnedAnnotation, VMOwnedMigrationAnnotation
                                                and its two values (added for Pass 2 forward-compat;
                                                unused until migration lands)

pkg/util/vmopv1/
├── vmowned_volumes.go                MODIFY — DiskModeForVolume, NormalizeDiskMode, IsDependentMode,
│                                              IsFcdRetained, EnsureCVIForPVC, ListCVIsForVM,
│                                              CVIVMInstanceUUIDIndexKey/CVIVMNameIndexKey + index
│                                              funcs, RemoveVMEntry
└── vmowned_volumes_snapshot.go       MODIFY — RemoveVMEntryIfNotRetained (shared by the detach
                                              controller, the sweeper, and Workflow E.5)

pkg/volumes/owned/
├── classify.go                       NEW — ClassifyVolumes: VolumePlan per PVC-backed spec volume
└── decision.go                       NEW — ResolveVolumeAction: recovery/idempotency decision table
                                              (VolumeAction from VolumeObservation)

controllers/virtualmachine/volumeattachdetach/
├── volumeattachdetach_controller.go       MODIFY — categorizeVolumeSpecs excludes every VM-owned
│                                                   disk (not just dependent) from the batch path;
│                                                   skipBatch widened to all modes; field indexes
│                                                   registered in AddToManager
└── volumeattachdetach_ownedvolumes.go     MODIFY — reconcileOwnedVolumeAttach (batches via
                                                   attachReadyDisks, writes vm.status.volumes with
                                                   Type: Managed and slot fields at attach time),
                                                   detachOwnedVolume (pairs by volumeName, splits
                                                   dependent/independent), removeCVIEntryIfNotRetained

controllers/virtualmachinesnapshot/
└── virtualmachinesnapshot_controller_ownedvolumes.go   MODIFY — refreshCVIDiskPathsFromSnapshot
                                                        (resolves via EnsureCVIForPVC, not the
                                                        VirtualDisk backing UUID — D.2 fix),
                                                        evaluateCVIForDeletedSnapshot

controllers/csivolumeinfo/
└── csivolumeinfo_controller.go       NEW — sweeper: removes a spec.vms entry whose VM does not
                                              resolve on a live (uncached) read, re-confirmed after
                                              a grace period

controllers/controllers.go            MODIFY — registers csivolumeinfo.AddToManager behind
                                              Features.VMOwnedVolumes

pkg/providers/
├── vm_provider_interface.go          MODIFY — AttachVolumeDisks, GetLiveDiskPathAtSlot added;
│                                              AttachOrphanedDiskToVM removed
├── vm_provider_volumes.go            MODIFY — VolumeDiskAddSpec, VolumeDiskPlacement
└── fake/fake_vm_provider.go          MODIFY — matching Fn overrides

pkg/providers/vsphere/
├── vmprovider_vm_ownedvolumes.go            MODIFY — AttachVolumeDisks (one ReconfigVM_Task per
│                                                    batch), per-disk vDiskId/CBT obligations,
│                                                    assertNoVMLevelCBT, GetLiveDiskPathAtSlot,
│                                                    DetachDiskAtSlot (fixed to not base-walk)
└── vmprovider_vm_ownedvolumes_snapshot.go   MODIFY — captureDroppedVolumeDiskPaths,
                                                    evaluateDroppedVolumeCVIEntries (E.5)

webhooks/virtualmachine/validation/
└── virtualmachine_validator.go       MODIFY — validateOwnedVolumeAttach widened to every disk
                                              mode + single-mode-per-volume invariant;
                                              validateSnapshot rejects revert-to-deleting-snapshot;
                                              validateAnnotation makes VMOwnedVolumesAnnotation
                                              immutable once set

webhooks/persistentvolumeclaim/validation/
└── persistentvolumeclaim_validator.go   MODIFY — isVMOwnedPVCDeleteDenied also denies on
                                                 non-empty spec.vms, not only VMManaged

pkg/vmconfig/volumes/unmanaged/register/
└── unmanagedvolumes_register.go      (pre-existing; verified, not modified this pass) —
                                              filterOutManagedPVCDisks excludes a real,
                                              non-placeholder PVC-backed disk from unmanaged
                                              registration by provenance (dataSourceRef), not state

pkg/providers/vsphere/vmlifecycle/
└── update_status.go                  (pre-existing; verified, not modified this pass) —
                                              Classic→Managed promotion for a non-FCD PVC-backed
                                              disk already covers the dependent-mode shape

config/rbac/role.yaml                 REGEN — csivolumeinfos (create/get/list/patch/update/watch),
                                              persistentvolumes (get); cnsnodevmbatchattachments
                                              already had every verb V11 (Pass 2) will need
```

---

## API / CRD strategy

### VM annotation

`vmoperator.vmware.com/vm-owned-volumes` (pre-existing constant, `pkg/constants/constants.go`) is stamped by the VM mutation webhook on create when `VMOwnedVolumes` is enabled. The validating webhook enforces immutability on the **transition**, not the **principal**: once set, any change to a non-empty value is rejected for every caller, admin or not — the absent → `"true"` transition remains open so the same mutation-webhook code path (and, later, a migration controller) can still perform it.

### CsiVolumeInfo external type

`CsiVolumeInfo` (`cns.vmware.com/v1alpha1`, namespace `vmware-system-csi`) is CSI's type. vm-operator's copy in `external/vsphere-csi-driver/api/v1alpha1/` is a byte-for-byte mirror of CSI's authoritative struct (confirmed against the real `vsphere-csi-driver` source, not the earlier draft), imported via the root `go.mod`'s `replace` directive and a checked-in `vendor/` tree that must be regenerated (`go mod vendor`) whenever the mirrored type changes.

**Two-channel writer contract (design convention, not webhook-enforced today):**
- vm-operator patches `spec.vms` (append/update this VM's entry) and `spec.diskPath` (JIT refresh before a dependent-mode detach) only. It never patches `status`.
- CSI patches `status.*`, `spec.diskUUID`, `spec.diskPath` (at Unregister time), and `spec.pvcName`/`spec.pvcNamespace` (on rebind).

### Feature gate

`pkgcfg.Features.VMOwnedVolumes` (pre-existing) gates every new code path. Default `false`. When `false`, the volume controller, snapshot controller, registration pass, and both validating webhooks fall through to their pre-feature behavior unchanged.

---

## Controller / webhook impact

### Volume controller (`controllers/virtualmachine/volumeattachdetach/`)

`categorizeVolumeSpecs` excludes **every** PVC-backed volume on a VM-owned-volumes VM from the batch path — not only dependent-mode ones — so such a VM never receives a `CnsNodeVMBatchAttachment` at all. `skipBatch` is `true` whenever the feature gate and the VM annotation are both set.

**Workflow A (attach)**, `reconcileOwnedVolumeAttach`:
1. `owned.ClassifyVolumes(vm)` produces a `VolumePlan` per PVC-backed spec volume not yet in `vm.status.volumes`.
2. `vmopv1util.EnsureCVIForPVC` resolves the CVI (creating it, owned by the `PersistentVolume`, if one does not yet exist — a missing CVI on a VM-owned VM is an anomaly to repair, not a PVC to skip). The VM's entry (`vmName`, `vmInstanceUUID`, `diskMode`, `volumeName`) is appended or updated in place if it drifted (self-healing for entries written before `volumeName` existed, or drift from a future migration/VKS conversion).
3. **Independent mode**: logged as pending — device attach is not yet implemented (see spec Non-goals) — and the reconcile requeues.
4. **Dependent mode**: once `vmopv1util.IsGreenSignal(cvi)` is true, the disk joins a batch. `attachReadyDisks` issues **one** `ReconfigVM_Task` for the whole batch (never one per disk), honoring `fcd-retained`'s observed `vDiskId` and per-disk `changeTrackingEnabled` (never the VM-level flag — that would destroy every other retained FCD's change ID). `vm.status.volumes` is written directly from the returned placements — `Type: Managed`, slot fields, `Attached: true` — at attach time, not deferred to a later observation pass, because detach's `volumeName` pairing depends on the slot being present the moment a volume leaves `vm.spec.volumes`.

**Workflow B (detach)**, `detachOwnedVolume`:
1. For a CVI entry for this VM whose `volumeName` is no longer in `vm.spec.volumes`, the matching `vm.status.volumes` entry (found by `volumeName`, never `diskUUID`) gives the device slot.
2. **Independent mode**: the VM entry is removed from `spec.vms` immediately — no device was ever added by vm-operator, so there is nothing to remove.
3. **Dependent mode**: `GetLiveDiskPathAtSlot` reads the device's *current* backing path (not base-walked — walking to the root VMDK would resolve the wrong path for a live detach), patches `CsiVolumeInfo.spec.diskPath`, and `DetachDiskAtSlot` issues `ReconfigVM_Task` to remove the disk (file preserved). On success, `removeCVIEntryIfNotRetained` removes the VM's entry unless a snapshot still retains the disk.

### Snapshot controller (`controllers/virtualmachinesnapshot/`)

**Workflow D (snapshot delete)**: `refreshCVIDiskPathsFromSnapshot` resolves each dropped disk's CVI via `EnsureCVIForPVC(pvcName)` — the fix for the D.2 bug where the prior code resolved by `disk.UUID` (the *VirtualDisk backing UUID*, not the CNS volume ID) and silently missed the CVI. `evaluateCVIForDeletedSnapshot` then removes the entry via the shared two-tier retention check (`vmopv1util.RemoveVMEntryIfNotRetained`) unless another snapshot or the live VM still holds the disk.

**Workflow E (revert)**: `captureDroppedVolumeDiskPaths` records the live backing path for every volume the revert will drop, before `restoreVMSpecFromSnapshot` runs. `evaluateDroppedVolumeCVIEntries` (E.5) — a new explicit step; previously this "happened to work" only as an accidental side effect of the old diskUUID-keyed detach path, which the finalized spec itself flags as broken once detach is re-keyed by `volumeName` — runs the same two-tier check after the revert completes.

### VM validating webhook (`webhooks/virtualmachine/validation/`)

- `validateOwnedVolumeAttach`: on an UPDATE that adds a new PVC-backed volume to a VM-owned-volumes VM, resolves the PVC → PV → CVI and enforces (a) the single-disk-mode-per-volume invariant across every VM already in `spec.vms`, regardless of access mode, and (b) the RWO concurrent-attach rejection for every mode (previously gated to `DiskMode==""||Persistent` only — an independent-mode volume could attach to two VMs at once with no admission pushback).
- `validateSnapshot`: rejects a newly-requested revert (not a repeat of one already in progress) whose target `VirtualMachineSnapshot` has a non-empty `metadata.deletionTimestamp`. A read error other than `NotFound` admits rather than rejects, so a transient failure here never blocks every edit to the VM.
- `validateAnnotation`: `VMOwnedVolumesAnnotation` is immutable once set, scoped to the value transition rather than the caller, per the API strategy above.

### PVC validating webhook (`webhooks/persistentvolumeclaim/validation/`)

`isVMOwnedPVCDeleteDenied` denies deletion when `status.ownership=VMManaged` (unchanged) **or** when `spec.vms` is non-empty (new) — the latter catches an attached independent-mode volume, which never reaches `VMManaged`.

### CsiVolumeInfo sweeper (`controllers/csivolumeinfo/`)

A backstop, not a control path: the volume controller removes its own entry on every normal detach or VM deletion. The sweeper only catches what that path missed — e.g. a VM CR deleted bypassing the finalizer. It resolves VM existence via `mgr.GetAPIReader()` (an uncached, live read) and requires the absence to hold for a grace period across two checks before acting, so a VM merely not yet in the watch cache is never mistaken for deleted.

### RBAC additions

Regenerated via `controller-gen ... rbac:roleName=manager-role` from the `+kubebuilder:rbac` markers already present on the touched controllers/webhooks:

| Resource | Group | Verbs added |
|----------|-------|-------------|
| `csivolumeinfos` | `cns.vmware.com` | create, get, list, patch, update, watch |
| `persistentvolumes` | core | get |

`cnsnodevmbatchattachments` already carries every verb (`create`/`delete`/`get`/`list`/`patch`/`update`/`watch`) that migration (V11, Pass 2) will need to freeze and retire a BA — no change required there.

---

## Test strategy

| Layer | Mechanism | Location |
|-------|-----------|----------|
| Unit (plain Go, vim25 device shaping) | `*_test.go` beside source | `pkg/providers/vsphere/vmprovider_vm_ownedvolumes_test.go`, `pkg/volumes/owned/decision_test.go` |
| Unit (Ginkgo, fake client) | `testlabels.Controller` | `controllers/virtualmachine/volumeattachdetach/volumeattachdetach_ownedvolumes_unit_test.go`, `controllers/virtualmachinesnapshot/virtualmachinesnapshot_controller_unit_test.go`, `controllers/csivolumeinfo/csivolumeinfo_controller_test.go`, `webhooks/virtualmachine/validation/virtualmachine_validator_unit_test.go`, `webhooks/persistentvolumeclaim/validation/persistentvolumeclaim_validator_unit_test.go` |
| Integration (envtest) | `testlabels.EnvTest` | `controllers/virtualmachine/volumeattachdetach/volumeattachdetach_controller_intg_test.go` (existing suite, exercised the new CVI CRD once the missing manifest was added) |
| E2E | Real WCP Supervisor | Deferred until independent-mode device attach lands — attach/detach for dependent mode is exercised by envtest + unit coverage today; a full E2E pass belongs with the follow-on migration spec once both passes are on a branch together |

---

## Rollout / migration

- **Feature gate**: `pkgcfg.Features.VMOwnedVolumes` defaults to `false`.
- **Annotation boundary**: only VMs created after the gate is enabled receive the annotation and route through this path. Pre-existing VMs remain brownfield and use the legacy path until the follow-on migration spec (Pass 2) converts them.
- **Gate rollback**: not supported while any VM-owned volume exists in any mode. Operators must detach everything first.
- **Partner comms**: CSI must already write `spec.vms[*].volumeName` (confirmed shipped in the real `vsphere-csi-driver` source as of this pass) and treat `spec.vms` non-empty as the "attached" signal for its own PVC-protection finalizer.
- **Release note**: extends an existing gated feature; no migration required for a fresh cluster. Gate remains off by default.

---

## Complexity tracking

| Item | Why needed | Simpler alternative rejected because |
|------|------------|--------------------------------------|
| `volumeName` as the detach correlation key, not `diskUUID` | `diskUUID` is empty for `fcd-retained` and ambiguous when two volumes are removed from `vm.spec.volumes` in one edit. | Keying on `diskUUID` was the v1 approach; it silently mispaired or dropped the wrong disk in exactly the two cases above. |
| One batched `ReconfigVM_Task` for all ready disks, not one per disk | vCenter serializes reconfigures per VM; batching is the only way to keep attach latency bounded as disk count grows. | Per-disk reconfigure is simpler to reason about per disk but does not scale and reintroduces a race window between disks in the same attach event. |
| Deferred independent-mode device attach | No verified CNS/vslm client or `PersistentVolume` field exists in this codebase to resolve the vSphere identifiers an independent attach needs; the plan's proposed mechanism (a PV `volumeAttributes` key) does not correspond to anything in CSI's real source. | Fabricating an unverified resolution mechanism to appear "done" would ship an untested, unreviewable device-attach path; better to land the CVI-entry half now and defer the device half to a change that can verify its inputs. |
| Mandatory vCenter snapshot-tree query in Workflow D/E | Unmanaged snapshots have no Kubernetes CR and are invisible to a CR-only scan; missing one would prematurely re-register an FCD a snapshot still pins. | A CR-only fast path alone would silently miss unmanaged snapshots. |
| External type in `external/vsphere-csi-driver/` sub-module, vendored | CsiVolumeInfo is CSI's type; mirroring it byte-for-byte in the external tree (rather than inlining a divergent copy) keeps the two in sync and makes drift a compile-time `go mod vendor` diff, not a silent runtime mismatch. | Inlining a hand-written approximation in the root module was the earlier draft's approach and had already drifted from CSI's real fields (missing `volumeName`) before this pass started. |
