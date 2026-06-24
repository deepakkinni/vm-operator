# Implementation Plan: VM-Owned Volume Attach/Detach

- **Spec**: [`spec.md`](./spec.md)
- **Epic**: vmop-TBD
- **Date**: 2026-06-23
- **Full external spec**: `cns-specs/VGL-62908/vm-owned-volume-attach-detach-spec.md`

---

## Summary

Implement the vm-operator side of the VM-owned volume lifecycle: feature gate, VM annotation stamping, volume controller changes for greenfield attach (Workflow A) and detach (Workflow B), snapshot deletion evaluation (Workflow D), revert evaluation (Workflow E), PVC deletion protection webhook, and the supporting external type (`CsiVolumeInfo`) in a new `external/vsphere-csi-driver` sub-module. All behavior is gated behind the `VMOwnedVolumes` feature gate; brownfield VMs (annotation absent) continue through the existing legacy path unchanged.

---

## Technical context

| Field | Value |
|-------|-------|
| **Language** | Go 1.22+ |
| **Primary dependencies** | `controller-runtime` v0.17, `govmomi` v0.36, `kubebuilder` v3 |
| **API server** | Kubernetes 1.30+ (vSphere Supervisor) |
| **Testing** | Ginkgo v2 + Gomega; `vcsim` for integration; real WCP Supervisor for E2E |
| **Code generation** | `controller-gen` (deepcopy, CRD manifests, RBAC markers) |
| **Target platform** | VMware vSphere Supervisor (WCP); namespace-isolated multi-tenancy |
| **API version(s) touched** | `vmoperator.vmware.com/v1alpha4` (VM), new external CRD `cns.vmware.com/v1alpha1` (CsiVolumeInfo) |
| **Modules touched** | Root module (`github.com/vmware-tanzu/vm-operator`), new `external/vsphere-csi-driver/` sub-module |
| **New dependencies** | None beyond existing govmomi and controller-runtime |

---

## Constitution check

| Rule | Status | Notes |
|------|--------|-------|
| API compatibility — additive only, no version bump | OK | New annotation constant + CsiVolumeInfo external type are additive. No existing VM API field is removed or renamed. |
| New CRD requires `+kubebuilder:object:root=true`, `+groupversion`, deepcopy | OK | CsiVolumeInfo lives in `external/vsphere-csi-driver/` (its own module) and is treated as an external type. `make generate-external-manifests` regenerates the CRD. |
| Thin controllers — business logic in `pkg/` | OK | Volume controller reconcile loop stays thin; greenfield logic extracted to `pkg/volumes/`. |
| No controller calls vSphere APIs directly | OK | All `ReconfigVM_Task` calls go through `pkg/providers/vsphere/`. |
| Controllers track `status.observedGeneration` and `Ready` condition | OK | CsiVolumeInfo status is owned by CSI; vm-operator reads it but never writes it. vm-operator owns `vm.status.volumes` conditions. |
| Fan-out via `controllerutil.CreateOrPatch` | OK | CVI entries are patched, not created by vm-operator (CSI creates CVIs). |
| Webhooks in `webhooks/`; CEL for simple rules, Go for complex | OK | PVC delete protection webhook is Go validation (needs CVI lookup). VM attach pre-validation is Go (needs CVI lookup for `spec.vms`). |
| Controllers for non-`vmoperator.vmware.com` API groups outside `controllers/` | OK | CsiVolumeInfo reconcile is owned by CSI, not vm-operator. vm-operator only reads/patches CVI. |
| One test file per package, one suite bootstrap, labels from `testlabels` | OK | All new test files follow the standard layout. |
| E2E coverage for every cluster-observable behavior change | OK | E2E tasks are included for each workflow. |
| No internal Broadcom URLs in tracked files | OK | Tickets referenced as `vmop-NNN`; wiki pages as `WIKI page <ID>`. |

---

## Project structure

### New external sub-module

```
external/vsphere-csi-driver/
├── go.mod                                      NEW — own Go module
├── api/
│   └── cns/v1alpha1/
│       ├── doc.go                              NEW — +groupName: cns.vmware.com
│       ├── csivolumeinfo_types.go              NEW — CsiVolumeInfo CRD types
│       ├── register.go                         NEW — scheme registration
│       └── zz_generated.deepcopy.go           REGEN — controller-gen deepcopy
└── config/
    └── crd/
        └── cns.vmware.com_csivolumeinfos.yaml REGEN — CRD manifest
```

### Root module additions and modifications

```
pkg/constants/
└── annotations.go                              MODIFY — add VMOwnedVolumes annotation constant

pkg/config/
└── features.go                                 MODIFY — add VMOwnedVolumes feature gate

pkg/volumes/
├── owned.go                                    NEW — greenfield attach/detach business logic
│   (appendCVIEntry, waitForGreenSignal,
│    refreshDiskPath, removeCVIEntry,
│    buildReconfigAddSpec, buildReconfigRemoveSpec)
└── owned_test.go                               NEW — unit tests

pkg/providers/vsphere/
└── vm_utils.go                                 MODIFY — expose findVirtualDiskBySlot helper
                                                         used by greenfield diskPath refresh

controllers/virtualmachine/volume/
└── volume_reconciler.go                        MODIFY — branch on VM annotation at reconcile entry;
                                                         call greenfield path for attach (Workflow A)
                                                         and detach (Workflow B)

controllers/virtualmachine/volume/
└── volume_reconciler_test.go                   MODIFY — add greenfield attach/detach unit tests

controllers/virtualmachine/snapshot/
└── snapshot_reconciler.go                      MODIFY — Workflow D: capture diskPaths,
                                                         DeleteSnapshot, evaluate entry removal;
                                                         Workflow E: capture diskPaths before revert,
                                                         evaluate dropped volumes after

controllers/virtualmachine/snapshot/
└── snapshot_reconciler_test.go                 MODIFY — add Workflow D and E unit tests

webhooks/virtualmachine/
└── validation_webhook.go                       MODIFY — RWO concurrent-attach check (spec.vms);
                                                         revert target not in deletion;
                                                         VolumeSnapshot CR pre-check for attach

webhooks/virtualmachine/
└── validation_webhook_test.go                  MODIFY — new webhook validation unit tests

webhooks/persistentvolumeclaim/
├── validation_webhook.go                       NEW — PVC DELETE protection: reject when
│                                                        CVI.status.ownership=VMManaged
└── validation_webhook_test.go                  NEW — unit tests

pkg/context/
└── vm_context.go                               MODIFY — carry CVI client reference if needed
                                                         (alternative: use manager client directly)

config/rbac/
└── role.yaml                                   MODIFY — add CsiVolumeInfo get/list/watch/patch
                                                         verbs; add PVC watch for PVC webhook

test/e2e/vmservice/vmownedvolumes/
└── vmownedvolumes_test.go                      NEW — E2E: attach, detach, snapshot delete,
                                                         revert, PVC delete protection
```

---

## API / CRD strategy

### VM annotation

A new constant `vmoperator.vmware.com/vm-owned-volumes: "true"` is added to `pkg/constants/annotations.go`. The annotation is stamped by the VM mutation webhook on create when the `VMOwnedVolumes` feature gate is enabled. It is immutable: the validation webhook rejects attempts to remove or modify it post-creation.

### CsiVolumeInfo external type

`CsiVolumeInfo` is a CSI-owned CRD (`cns.vmware.com/v1alpha1`, namespace `vmware-system-csi`). vm-operator treats it as an **external type** — it lives in `external/vsphere-csi-driver/` following the same pattern as `external/storage-policy-quota/`, `external/byok/`, etc. vm-operator's root `go.mod` imports it via a workspace `replace` directive.

**Two-channel writer contract (enforced by design, not by webhook):**
- vm-operator patches `spec.vms` and `spec.diskPath` only. It never patches CVI `status`.
- CSI patches `status.*`, `spec.diskUUID`, `spec.diskPath` (at Unregister), and `spec.pvc`/`spec.pvcNamespace` (on rebind).

### Feature gate

`pkgcfg.Features.VMOwnedVolumes` is added to `pkg/config/features.go`. Default is `false`. When `false`, all volume controller and snapshot controller code paths fall through to the existing legacy path.

---

## Controller / webhook impact

### Volume controller (`controllers/virtualmachine/volume/volume_reconciler.go`)

The volume controller's reconcile loop gains a branch at its entry point:

```
if vm.Annotations[VMOwnedVolumes] == "true" && feature gate enabled:
    → greenfield path (pkg/volumes/owned.go)
else:
    → existing legacy path (CnsNodeVMBatchAttachment)
```

**Workflow A (attach):**
1. For each PVC in `vm.spec.volumes` in dependent-persistent mode not yet reflected in `vm.status.volumes`: look up CVI via `PV.spec.csi.volumeHandle`. Append VM entry to `CVI.spec.vms` (patch, idempotent).
2. Re-read CVI. If green signal (`status.ownership=VMManaged`, `status.observedGeneration >= metadata.generation`, `status.phase=Succeeded`): read `spec.diskPath` and issue `ReconfigVM_Task` add.
3. Update `vm.status.volumes`.

**Workflow B (detach):**
1. For each PVC in `CVI.spec.vms` (this VM's entry) but absent from `vm.spec.volumes`: find the VirtualDisk device by slot from `vm.status.volumes`. Read `Backing.FileName`. Patch `CVI.spec.diskPath`.
2. Issue `ReconfigVM_Task` remove (keep file). On error: surface on `vm.status.volumes`, retain entry, retry.
3. After successful ReconfigVM remove: patch CVI to remove this VM's entry from `spec.vms`.
4. Update `vm.status.volumes`.

### Snapshot controller (`controllers/virtualmachine/snapshot/snapshot_reconciler.go`)

**Workflow D (snapshot delete) — modifications to existing deletion handler:**
1. Before calling `DeleteSnapshot`: read the VMSnap's frozen `pvc.disk.data`, resolve each PVC to its CVI, capture `diskPath` from the snapshot's device config (match by `Backing.Uuid`), patch `CVI.spec.diskPath`.
2. Call `DeleteSnapshot`.
3. For each disk: evaluate removal via two-tier check (VMSnap CR fast path + mandatory vCenter snapshot-tree query covering unmanaged snapshots). If neither the disk is on the VM nor any snapshot retains it, remove the VM entry from `CVI.spec.vms`.
4. Remove vm-operator finalizer.

**Workflow E (revert) — modifications to existing revert handler:**
1. Before calling `RevertToSnapshot`: compute `droppedVolumes = liveSpec − snapSpec`. For each dropped volume, capture `diskPath` from live VM device, patch `CVI.spec.diskPath`.
2. Call `RevertToSnapshot`. Run `restoreVMSpecFromSnapshot`.
3. For each dropped volume: evaluate via two-tier check (fast path + vCenter backstop). Remove entry if no snapshot retains the disk.

### VM mutation webhook (`webhooks/virtualmachine/`)

Add to mutating webhook (on create):
- When `VMOwnedVolumes` feature gate is enabled: stamp annotation `vmoperator.vmware.com/vm-owned-volumes: "true"`.

Add to validating webhook:
- **Annotation immutability:** reject attempts to modify or remove `vmoperator.vmware.com/vm-owned-volumes`.
- **Greenfield attach pre-validation:** when a PVC is being added to a greenfield VM's `spec.volumes` in dependent-persistent mode, check: (a) no `VolumeSnapshot` CRs exist for the PVC; (b) if RWO, CVI `spec.vms` does not contain a different VM.
- **Revert pre-validation:** reject if target VMSnap has a non-empty `metadata.deletionTimestamp`.

### PVC validating webhook (`webhooks/persistentvolumeclaim/`)

New `ValidatingAdmissionWebhook` on PVC DELETE:
- Resolve PVC → PV → `volumeHandle` → CVI name (`cns-volume-<volumeID>`) in `vmware-system-csi`.
- If CVI exists and `status.ownership=VMManaged`: reject with `Cannot delete PVC <name>: volume is VM-managed. Detach the volume from the VM or delete all retaining snapshots first.`
- If CVI absent or `status.ownership=CSIManaged`: allow.

### VM finalizer

`vmoperator.vmware.com/cvi-cleanup` is added to every greenfield VM. On VM deletion, before the finalizer is released, vm-operator iterates all CVIs containing an entry for this `vmInstanceUUID`, drives any in-flight detach to completion (or removes orphan entries where the disk is not on the VM and no snapshot retains it), and then releases the finalizer.

### RBAC additions

| Resource | Group | Verbs added |
|----------|-------|-------------|
| `csivolumeinfos` | `cns.vmware.com` | get, list, watch, patch |
| `csivolumeinfos/status` | `cns.vmware.com` | get, list, watch |
| `persistentvolumes` | core | get, list, watch |
| `persistentvolumeclaims` | core | watch (for PVC webhook) |

---

## Implementation phases

### Phase 1 — Foundation

Establish the scaffolding all subsequent phases depend on: external type, feature gate, annotation constant, mutation webhook change, and RBAC. No attach/detach logic.

**Deliverables:**
- `external/vsphere-csi-driver/` sub-module with `CsiVolumeInfo` types and generated deepcopy + CRD manifest.
- `pkgcfg.Features.VMOwnedVolumes` feature gate in `pkg/config/features.go`.
- `vmoperator.vmware.com/vm-owned-volumes` annotation constant in `pkg/constants/annotations.go`.
- VM mutation webhook stamps the annotation on create when the gate is enabled.
- VM validation webhook enforces annotation immutability.
- RBAC additions in `config/rbac/role.yaml`.

### Phase 2 — Workflow A (Attach)

Greenfield attach in the volume controller. Depends on Phase 1.

**Deliverables:**
- `pkg/volumes/owned.go`: `appendCVIEntry`, `waitForGreenSignal`, `buildReconfigAddSpec`.
- `controllers/virtualmachine/volume/volume_reconciler.go`: greenfield branch and Workflow A steps.
- Unit tests and integration tests (vcsim) for Workflow A.

### Phase 3 — Workflow B (Detach)

Greenfield detach in the volume controller. Depends on Phase 2.

**Deliverables:**
- `pkg/volumes/owned.go`: `refreshDiskPath`, `removeCVIEntry`, `buildReconfigRemoveSpec`.
- `controllers/virtualmachine/volume/volume_reconciler.go`: Workflow B steps.
- VM finalizer (`vmoperator.vmware.com/cvi-cleanup`) + CVI sweeper.
- Unit tests and integration tests (vcsim) for Workflow B.

### Phase 4 — Workflow D (Snapshot Delete)

Snapshot deletion evaluation. Depends on Phase 1; may proceed in parallel with Phases 2–3.

**Deliverables:**
- `controllers/virtualmachine/snapshot/snapshot_reconciler.go`: diskPath capture before DeleteSnapshot, two-tier entry-removal evaluation.
- Unit tests and integration tests for Workflow D.

### Phase 5 — Workflow E (Revert)

Revert evaluation. Depends on Phase 4 (shares evaluation logic).

**Deliverables:**
- `controllers/virtualmachine/snapshot/snapshot_reconciler.go`: diskPath capture before RevertToSnapshot, dropped-volume evaluation.
- VM validation webhook: reject revert if target VMSnap has `deletionTimestamp`.
- Unit tests and integration tests for Workflow E.

### Phase 6 — Webhooks and PVC Delete Protection

VM attach pre-validation and PVC delete protection. Depends on Phase 1.

**Deliverables:**
- VM validating webhook: RWO concurrent-attach check (CVI `spec.vms`), VolumeSnapshot CR pre-check.
- `webhooks/persistentvolumeclaim/validation_webhook.go`: PVC DELETE protection.
- Unit tests for both webhooks.
- E2E tests covering all user stories.

---

## Test strategy

| Layer | Mechanism | Location |
|-------|-----------|----------|
| Unit | `*_test.go` beside source, `testlabels.Controller` label | `pkg/volumes/owned_test.go`, `controllers/virtualmachine/volume/volume_reconciler_test.go`, `controllers/virtualmachine/snapshot/snapshot_reconciler_test.go`, `webhooks/virtualmachine/validation_webhook_test.go`, `webhooks/persistentvolumeclaim/validation_webhook_test.go` |
| Integration | `testlabels.EnvTest` or `testlabels.VCSim` | `test/intg/vmownedvolumes/` (attach, detach, snapshot delete, revert, PVC delete protection) |
| E2E | Ginkgo, real WCP Supervisor | `test/e2e/vmservice/vmownedvolumes/vmownedvolumes_test.go` |

**Mandatory E2E scenarios (per `e2e-sync-with-changes.md`):**
- Greenfield VM creation stamps the annotation.
- Workflow A: PVC attached, disk becomes plain VMDK on VM.
- Workflow B: PVC detached, FCD re-registered.
- Workflow D: snapshot deleted, orphaned disk re-registered.
- Workflow E: revert drops a volume, orphaned disk re-registered.
- PVC delete blocked while VM-managed.
- Legacy path unchanged for brownfield VM.

---

## Rollout / migration

- **Feature gate**: `pkgcfg.Features.VMOwnedVolumes` defaults to `false`. CSP admin enables it cluster-wide.
- **Greenfield boundary**: Only VMs created after the gate is enabled receive the annotation. Pre-existing VMs remain brownfield and use the legacy path.
- **Gate rollback**: Not supported while VM-owned volumes exist. Operators must detach all VM-owned volumes (re-registering FCDs) before disabling the gate.
- **Partner comms**: CSI driver must be updated to create `CsiVolumeInfo` CRs at PVC provisioning time and to reconcile the declarative ownership model before this gate is enabled. The gate and the CSI update should be coordinated in the same release.
- **Release note**: New feature. No migration required for existing VMs. Gate is off by default.

---

## Complexity tracking

| Item | Why needed | Simpler alternative rejected because |
|------|------------|-------------------------------------|
| Two-channel writer contract on CsiVolumeInfo | Prevents concurrent spec+status writes across two separate controllers (vm-operator and CSI) without a coordination lock. | Single-writer ownership would require vm-operator to manage CSI's Unregister/Register calls, crossing the separation-of-concerns boundary. |
| JIT diskPath resolution (no proactive freshness) | `diskPath` changes on storage vMotion; the only authoritative sources are the live VM device (pre-detach) and the snapshot device config (pre-snapshot-delete). | Proactive watch for `diskPath` changes would require an additional vSphere event listener and a continuous reconcile loop with no clear trigger. |
| Mandatory vCenter snapshot-tree query in D.4/E.5 | Unmanaged snapshots have no Kubernetes CR and are invisible to a CR-only scan. Missing them would cause premature FCD re-registration while a snapshot still pins the disk. | Relying on VMSnap CR scan alone would silently miss unmanaged snapshots, breaking the invariant that `spec.vms` is non-empty whenever any relationship exists. |
| External type in `external/vsphere-csi-driver/` sub-module | CsiVolumeInfo is owned by the CSI driver; vm-operator only reads/patches it. Placing it in the external tree follows the established pattern for cross-component types and keeps the schema authoritative in one place. | Inlining the type in the root module would create an implicit dependency inversion — vm-operator would "own" a type that CSI is the authoritative writer of. |
