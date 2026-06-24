# Tasks: VM-Owned Volume Attach/Detach

- **Spec**: [`spec.md`](./spec.md)
- **Plan**: [`plan.md`](./plan.md)
- **Epic**: vmop-TBD

Tasks within a Phase that are marked `[P]` may run in parallel. Each task that produces shipping code carries a `[vmop-NNN]` tag pointing to the JIRA story or sub-task linked to the epic via `customfield_10830`. Tags marked `TBD` will be filled once stories are filed under the epic.

---

## Phase 1 — Foundation

Dependencies: none. Tasks T002–T006 may run in parallel once T001 is merged.

- [ ] T001 [vmop-TBD] Scaffold `external/vsphere-csi-driver/` sub-module: `go.mod`, `api/cns/v1alpha1/doc.go` (`+groupName: cns.vmware.com`), `csivolumeinfo_types.go` (CsiVolumeInfo struct with spec/status fields per §4.1 of the full spec), `register.go`, `zz_generated.deepcopy.go` (via `make generate-external-manifests`), and `config/crd/cns.vmware.com_csivolumeinfos.yaml`. Wire into root `go.work` and root `go.mod` `replace` directive — `external/vsphere-csi-driver/`

- [ ] T002 [P] [vmop-TBD] Add `VMOwnedVolumes` feature gate to `pkg/config/features.go` (default `false`). Add gate check helpers. Unit test in `pkg/config/features_test.go` — `pkg/config/features.go`

- [ ] T003 [P] [vmop-TBD] Add annotation constant `vmoperator.vmware.com/vm-owned-volumes` to `pkg/constants/annotations.go`. Update any existing annotation constant index files as needed — `pkg/constants/annotations.go`

- [ ] T004 [P] [vmop-TBD] Stamp annotation in VM mutation webhook: when `VMOwnedVolumes` feature gate is enabled and a new VM is created, set `vmoperator.vmware.com/vm-owned-volumes: "true"`. Add immutability enforcement in VM validating webhook: reject updates that remove or change the annotation. Unit tests — `webhooks/virtualmachine/mutation/mutation_webhook.go`, `webhooks/virtualmachine/validation/validation_webhook.go`, `webhooks/virtualmachine/mutation/mutation_webhook_test.go`, `webhooks/virtualmachine/validation/validation_webhook_test.go`

- [ ] T005 [P] [vmop-TBD] Extend `config/rbac/role.yaml` with RBAC for CsiVolumeInfo: `get`, `list`, `watch`, `patch` on `csivolumeinfos` and `csivolumeinfos/status` in group `cns.vmware.com`. Add `get`, `list`, `watch` on `persistentvolumes` (if not already present) — `config/rbac/role.yaml`

- [ ] T006 [P] [vmop-TBD] Wire the `external/vsphere-csi-driver` module scheme into the manager's scheme builder so CsiVolumeInfo objects are readable/patchable via the manager client — `main.go` or `cmd/manager/main.go`

---

## Phase 2 — Workflow A (Greenfield Attach)

Dependencies: Phase 1. T011–T012 may run in parallel.

- [ ] T010 [vmop-TBD] Author `pkg/volumes/owned.go` with: `appendCVIEntry` (idempotent append of `{vmName, vmInstanceUUID}` to `CVI.spec.vms`), `isGreenSignal` (checks `status.ownership=VMManaged`, `status.observedGeneration >= metadata.generation`, `status.phase=Succeeded`), `buildReconfigAddSpec` (builds `VirtualMachineConfigSpec` for add-disk-as-plain-VMDK). Unit tests in `pkg/volumes/owned_test.go` — `pkg/volumes/owned.go`, `pkg/volumes/owned_test.go`

- [ ] T011 [P] [vmop-TBD] Modify `controllers/virtualmachine/volume/volume_reconciler.go`: at the reconcile entry, branch on VM annotation + feature gate. For greenfield VMs: for each PVC in `vm.spec.volumes` (dependent-persistent mode, not yet in `vm.status.volumes`), call `appendCVIEntry` then check `isGreenSignal`; if green, call `buildReconfigAddSpec` and issue `ReconfigVM_Task` via vmprovider. Update `vm.status.volumes` on success. Implement RWO concurrent-attach defense-in-depth check (reconciler level, in addition to webhook) — `controllers/virtualmachine/volume/volume_reconciler.go`

- [ ] T012 [P] [vmop-TBD] Unit tests for greenfield attach in volume reconciler: first attach (entry absent → append → wait for green → ReconfigVM), idempotent re-entry (entry exists → skip append → check signal), RWO concurrent attach rejected at reconciler level, brownfield VM falls through to legacy path — `controllers/virtualmachine/volume/volume_reconciler_test.go`

- [ ] T013 [vmop-TBD] Integration tests (vcsim, `testlabels.VCSim`) for Workflow A: greenfield VM + PVC → CVI entry appended → green signal present → disk added to vcsim VM. Idempotent re-reconcile. RWO concurrent attach rejected — `test/intg/vmownedvolumes/attach_intg_test.go`

---

## Phase 3 — Workflow B (Greenfield Detach)

Dependencies: Phase 2. T021–T022 may run in parallel.

- [ ] T020 [vmop-TBD] Extend `pkg/volumes/owned.go` with: `refreshDiskPath` (finds VirtualDisk by slot from `vm.status.volumes`, reads `Backing.FileName`, patches `CVI.spec.diskPath`), `removeCVIEntry` (removes this VM's entry from `CVI.spec.vms`), `buildReconfigRemoveSpec` (builds `VirtualMachineConfigSpec` for remove-disk-keep-file). Unit tests — `pkg/volumes/owned.go`, `pkg/volumes/owned_test.go`

- [ ] T021 [P] [vmop-TBD] Modify `controllers/virtualmachine/volume/volume_reconciler.go`: Workflow B steps — for each CVI entry for this VM whose PVC is absent from `vm.spec.volumes`: call `refreshDiskPath`, issue `ReconfigVM_Task` remove; on success call `removeCVIEntry`. Surface errors on `vm.status.volumes`. Implement idempotency: if disk already off VM and PVC absent from spec, skip ReconfigVM and go straight to `removeCVIEntry` — `controllers/virtualmachine/volume/volume_reconciler.go`

- [ ] T022 [P] [vmop-TBD] Unit tests for greenfield detach: normal detach (refreshDiskPath → ReconfigVM remove → removeCVIEntry → last entry triggers CSI Register), ReconfigVM blocked by snapshot (entry stays, error surfaced), crash-recovery idempotency (disk already off VM → skip ReconfigVM → removeCVIEntry) — `controllers/virtualmachine/volume/volume_reconciler_test.go`

- [ ] T023 [vmop-TBD] Add VM finalizer `vmoperator.vmware.com/cvi-cleanup` to every greenfield VM at reconcile time. On VM deletion, before releasing the finalizer, iterate all CVIs with an entry for this `vmInstanceUUID`: if disk is on VM, drive ReconfigVM remove; if disk is off VM and no snapshot retains it, remove the entry; then release the finalizer. Unit tests — `controllers/virtualmachine/volume/volume_reconciler.go`, `controllers/virtualmachine/volume/volume_reconciler_test.go`

- [ ] T024 [vmop-TBD] Implement CVI sweeper: a periodic reconcile loop that lists `CsiVolumeInfo` CRs and for each entry whose `vmInstanceUUID` does not resolve to an existing VM CR, removes the entry. Gate on feature flag. Unit tests — `pkg/volumes/owned.go` or new `pkg/volumes/sweeper.go`, corresponding `_test.go`

- [ ] T025 [vmop-TBD] Integration tests (vcsim, `testlabels.VCSim`) for Workflow B: detach triggers diskPath refresh + ReconfigVM remove + entry removal + CSI auto-Register. VM deletion mid-detach cleaned up by finalizer. Sweeper removes orphan entries — `test/intg/vmownedvolumes/detach_intg_test.go`

---

## Phase 4 — Workflow D (Snapshot Delete)

Dependencies: Phase 1 (CsiVolumeInfo type). May proceed in parallel with Phases 2–3.

- [ ] T030 [vmop-TBD] Extend `pkg/volumes/owned.go` with: `captureDiskPathsFromSnapshot` (reads VMSnap frozen `pvc.disk.data`, resolves PVC → PV → volumeID → CVI, reads snapshot device config for `Backing.FileName`, patches `CVI.spec.diskPath`), `evaluateCVIEntryRemoval` (two-tier check: fast path VMSnap CR `pvc.disk.data` + mandatory vCenter snapshot-tree query for unmanaged snapshots). Unit tests — `pkg/volumes/owned.go`, `pkg/volumes/owned_test.go`

- [ ] T031 [vmop-TBD] Modify `controllers/virtualmachine/snapshot/snapshot_reconciler.go`: in the VMSnap deletion handler, before `DeleteSnapshot`, call `captureDiskPathsFromSnapshot`. After `DeleteSnapshot`, call `evaluateCVIEntryRemoval` for each disk. Remove vm-operator finalizer after all disks processed. Idempotency: if vSphere snapshot already gone, skip to evaluation; if CVI already processed, skip — `controllers/virtualmachine/snapshot/snapshot_reconciler.go`

- [ ] T032 [vmop-TBD] Unit tests for Workflow D: diskPath captured from snapshot device config (not stale `pvc.disk.data`), entry preserved when another snapshot retains disk, entry removed when no snapshot retains disk and disk not on VM, unmanaged snapshot check via vCenter backstop — `controllers/virtualmachine/snapshot/snapshot_reconciler_test.go`

- [ ] T033 [vmop-TBD] Integration tests (vcsim, `testlabels.VCSim`) for Workflow D: snapshot delete releases orphaned disk via CSI Register; shared disk retained by second snapshot; crash recovery — `test/intg/vmownedvolumes/snapshot_delete_intg_test.go`

---

## Phase 5 — Workflow E (Revert)

Dependencies: Phase 4 (shares `evaluateCVIEntryRemoval`). T041–T042 may run in parallel.

- [ ] T040 [vmop-TBD] Modify `controllers/virtualmachine/snapshot/snapshot_reconciler.go`: in the revert handler, before `RevertToSnapshot`, compute `droppedVolumes = liveSpec − snapSpec`; for each dropped volume, look up CVI and call `refreshDiskPath` (live VM device). After `RevertToSnapshot` and `restoreVMSpecFromSnapshot`, call `evaluateCVIEntryRemoval` for each dropped volume. Do not check `removable` (revert is controller-driven, not a user edit) — `controllers/virtualmachine/snapshot/snapshot_reconciler.go`

- [ ] T041 [P] [vmop-TBD] Unit tests for Workflow E: diskPaths captured before revert, dropped volume with no snapshot retained → entry removed → CSI Register, dropped volume retained by another snapshot → entry stays, re-adopted volumes (already in `spec.vms`) are no-ops — `controllers/virtualmachine/snapshot/snapshot_reconciler_test.go`

- [ ] T042 [P] [vmop-TBD] Add revert pre-validation to VM validating webhook: reject `vm.spec.currentSnapshotName` change when the target `VirtualMachineSnapshot` has a non-empty `metadata.deletionTimestamp`. Unit tests — `webhooks/virtualmachine/validation/validation_webhook.go`, `webhooks/virtualmachine/validation/validation_webhook_test.go`

- [ ] T043 [vmop-TBD] Integration tests (vcsim, `testlabels.VCSim`) for Workflow E: revert drops a volume that is re-registered as FCD; revert drops a volume retained by another snapshot (entry preserved); revert to deleting snapshot rejected — `test/intg/vmownedvolumes/revert_intg_test.go`

---

## Phase 6 — Webhooks and PVC Delete Protection

Dependencies: Phase 1. T050–T052 may run in parallel.

- [ ] T050 [P] [vmop-TBD] Add VM attach pre-validation to VM validating webhook: for a greenfield VM, when a PVC is added to `vm.spec.volumes` in dependent-persistent mode, check: (a) no `VolumeSnapshot` CRs reference the PVC, (b) if RWO, `CsiVolumeInfo.spec.vms` does not already contain a different VM. Unit tests — `webhooks/virtualmachine/validation/validation_webhook.go`, `webhooks/virtualmachine/validation/validation_webhook_test.go`

- [ ] T051 [P] [vmop-TBD] Author `webhooks/persistentvolumeclaim/validation_webhook.go`: `ValidatingAdmissionWebhook` on PVC DELETE; resolve PVC → PV → `volumeHandle` → CVI name; if CVI exists and `status.ownership=VMManaged`, reject with descriptive error. Register webhook in `main.go` / webhook suite. Unit tests — `webhooks/persistentvolumeclaim/validation_webhook.go`, `webhooks/persistentvolumeclaim/validation_webhook_test.go`

- [ ] T052 [P] [vmop-TBD] Register PVC webhook in `config/rbac/`, `config/webhook/`, and `webhooks/` suite bootstrap. Generate webhook manifests — `config/webhook/manifests.yaml`, `webhooks/suite_test.go`

- [ ] T053 [vmop-TBD] E2E tests (`test/e2e/vmservice/vmownedvolumes/vmownedvolumes_test.go`): greenfield VM creation stamps annotation; Workflow A (PVC attached, disk is plain VMDK); Workflow B (PVC detached, FCD re-registered); Workflow D (snapshot deleted, orphan disk re-registered); Workflow E (revert drops volume, disk re-registered); PVC delete blocked while VM-managed; brownfield VM uses legacy path unchanged; feature gate off produces no annotation — `test/e2e/vmservice/vmownedvolumes/vmownedvolumes_test.go`

---

## Phase Final — Polish

Dependencies: All prior phases.

- [ ] T060 [vmop-TBD] Update `docs/` with user-facing documentation for VM-owned volumes: feature gate configuration, greenfield/brownfield boundary, annotation semantics, attach/detach prerequisites, PVC deletion guard — `docs/`

- [ ] T061 [vmop-TBD] Remove `Epic: TBD` from `spec.md` header once epic ticket is filed. Update `tasks.md` `[vmop-TBD]` tags once story/sub-task tickets are created under the epic — `.sdd/specs/002-vm-owned-volumes/spec.md`, `.sdd/specs/002-vm-owned-volumes/tasks.md`
