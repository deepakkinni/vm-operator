# Tasks: VM-Owned Volume Attach/Detach

- **Spec**: [`spec.md`](./spec.md)
- **Plan**: [`plan.md`](./plan.md)
- **Epic**: vmop-TBD

Tasks within a Phase marked `[P]` may run in parallel. Each task that produces shipping code carries a `[vmop-NNN]` tag pointing to the JIRA story or sub-task linked to the epic via `customfield_10830`; tags marked `TBD` will be filled once stories are filed under the epic. This list reflects what actually shipped, commit by commit, on `topic/dk016388/vmown-impl-v2` — see `cns-specs/VGL-62908/implementation/vmop.md` §18 for the authoritative sequencing table this mirrors.

---

## Phase 1 — CsiVolumeInfo type alignment

Dependencies: none. Blocked on CSI shipping `spec.vms[*].volumeName` in its authoritative type (confirmed shipped).

- [x] T001 [vmop-TBD] Re-align `external/vsphere-csi-driver/api/v1alpha1/csivolumeinfo_types.go` byte-for-byte with CSI's authoritative `CsiVolumeInfo` type, including `spec.vms[*].volumeName`; hand-fix `zz_generated.deepcopy.go` (the `object` generator panics on this codebase); regenerate the previously-missing `config/crd/external-crds/cns.vmware.com_csivolumeinfos.yaml` via `controller-gen crd`; regenerate `vendor/` — `external/vsphere-csi-driver/api/v1alpha1/csivolumeinfo_types.go`, `external/vsphere-csi-driver/api/v1alpha1/zz_generated.deepcopy.go`, `config/crd/external-crds/cns.vmware.com_csivolumeinfos.yaml`

## Phase 2 — Route every disk mode through the CVI path

Dependencies: Phase 1.

- [x] T010 [vmop-TBD] Widen `categorizeVolumeSpecs`/`skipBatch` in `controllers/virtualmachine/volumeattachdetach/volumeattachdetach_controller.go` so a VM-owned-volumes VM never receives a `CnsNodeVMBatchAttachment`, in any disk mode — `controllers/virtualmachine/volumeattachdetach/volumeattachdetach_controller.go`
- [x] T011 [P] [vmop-TBD] Add `pkg/volumes/owned/classify.go` (`VolumePlan`, `ClassifyVolumes`) to classify every PVC-backed spec volume by disk mode, independent of dependent/independent — `pkg/volumes/owned/classify.go`

## Phase 3 — Resolve CsiVolumeInfo without cluster-wide scans

Dependencies: Phase 1.

- [x] T020 [vmop-TBD] Add `EnsureCVIForPVC` (create-on-demand, owned by the `PersistentVolume`), `ListCVIsForVM`, and the `CVIVMInstanceUUIDIndexKey`/`CVIVMNameIndexKey` field indexes to `pkg/util/vmopv1/vmowned_volumes.go`; register the indexes in `AddToManager` — `pkg/util/vmopv1/vmowned_volumes.go`, `controllers/virtualmachine/volumeattachdetach/volumeattachdetach_controller.go`

## Phase 4 — Batch VM-owned disk attach into one reconfigure

Dependencies: Phase 2, Phase 3.

- [x] T030 [vmop-TBD] Add `providers.VMProviderInterface.AttachVolumeDisks` (batched) and `VolumeDiskAddSpec`/`VolumeDiskPlacement`; remove the superseded `AttachOrphanedDiskToVM` — `pkg/providers/vm_provider_interface.go`, `pkg/providers/vm_provider_volumes.go`, `pkg/providers/fake/fake_vm_provider.go`
- [x] T031 [vmop-TBD] Implement `AttachVolumeDisks` in `pkg/providers/vsphere/vmprovider_vm_ownedvolumes.go`: one `ReconfigVM_Task` per batch, `findDiskByBackingPath`, `buildVolumeDisk`, `findControllerByTypeAndBus`, `diskModeToVim`, `placementFromDisk`. Unit tests as plain Go (no Ginkgo — this is vim25 device shaping, not reconcile behavior) — `pkg/providers/vsphere/vmprovider_vm_ownedvolumes.go`, `pkg/providers/vsphere/vmprovider_vm_ownedvolumes_test.go`
- [x] T032 [vmop-TBD] Wire `reconcileOwnedVolumeAttach`/`attachReadyDisks` in the volume controller: resolve/write the CVI entry, wait for the green signal (dependent mode), batch ready disks, issue the reconfigure, and write `vm.status.volumes` (`Type: Managed`, slot fields) from the returned placements at attach time — `controllers/virtualmachine/volumeattachdetach/volumeattachdetach_ownedvolumes.go`

## Phase 5 — Honour FCD attach obligations on raw reconfigure

Dependencies: Phase 4.

- [x] T040 [vmop-TBD] Carry the observed `vDiskId` through for an `fcd-retained` disk (`vmopv1util.IsFcdRetained`) so the platform's linked-clone precheck has a valid FCD identity; set per-disk `changeTrackingEnabled` (never the VM-level flag — `assertNoVMLevelCBT` guards against a regression that would destroy every other retained FCD's change ID) — `pkg/providers/vsphere/vmprovider_vm_ownedvolumes.go`

## Phase 6 — Detach VM-owned disks by volume name, not disk UUID

Dependencies: Phase 4. Lands as one commit — the recovery decision table depends on the same correlation key.

- [x] T050 [vmop-TBD] Add `GetLiveDiskPathAtSlot` (current backing path, not base-walked) and fix `DetachDiskAtSlot` to use it instead of the base-walking helper — `pkg/providers/vsphere/vmprovider_vm_ownedvolumes.go`
- [x] T051 [vmop-TBD] Rewrite `detachOwnedVolume` to pair a CVI entry to its `vm.status.volumes` entry by `volumeName`, split dependent (refresh diskPath, reconfigure, evaluate retention) from independent (remove the entry immediately, no device operation) — `controllers/virtualmachine/volumeattachdetach/volumeattachdetach_ownedvolumes.go`
- [x] T052 [P] [vmop-TBD] Add `pkg/volumes/owned/decision.go` (`VolumeAction`, `VolumeObservation`, `ResolveVolumeAction`): the idempotency/recovery decision table for every combination of live-disk presence, CVI entry, and spec-volume presence — `pkg/volumes/owned/decision.go`, `pkg/volumes/owned/decision_test.go`

## Phase 7 — Correct snapshot-delete and revert CVI evaluation

Dependencies: Phase 3.

- [x] T060 [vmop-TBD] Fix `refreshCVIDiskPathsFromSnapshot` (D.2): resolve the CVI via `EnsureCVIForPVC(pvcName)` instead of `disk.UUID` (the VirtualDisk backing UUID, not the CNS volume ID — the prior code silently missed the CVI) — `controllers/virtualmachinesnapshot/virtualmachinesnapshot_controller_ownedvolumes.go`
- [x] T061 [P] [vmop-TBD] Add the explicit E.5 step (`evaluateDroppedVolumeCVIEntries`) wired into the post-revert flow after `restoreVMSpecFromSnapshot`, using the shared `vmopv1util.RemoveVMEntryIfNotRetained` two-tier check — `pkg/providers/vsphere/vmprovider_vm_ownedvolumes_snapshot.go`, `pkg/providers/vsphere/vmprovider_vmsnapshot.go`

## Phase 8 — Extend VM-owned volume admission checks

Dependencies: Phase 6 (single-mode invariant reads `spec.vms[*].diskMode` written there).

- [x] T070 [vmop-TBD] Widen the attach-time check in `validateVolumes` to every disk mode (was `DiskMode==""||Persistent` only) — `webhooks/virtualmachine/validation/virtualmachine_validator.go`
- [x] T071 [P] [vmop-TBD] Add the single-disk-mode-per-volume invariant to `validateOwnedVolumeAttach` — `webhooks/virtualmachine/validation/virtualmachine_validator.go`
- [x] T072 [P] [vmop-TBD] Reject a revert whose target snapshot has a non-empty `deletionTimestamp` in `validateSnapshot`, only when a revert is not already in progress — `webhooks/virtualmachine/validation/virtualmachine_validator.go`
- [x] T073 [P] [vmop-TBD] Make `VMOwnedVolumesAnnotation` immutable once set (transition-scoped, not principal-scoped) in `validateAnnotation` — `webhooks/virtualmachine/validation/virtualmachine_validator.go`
- [x] T074 [P] [vmop-TBD] Deny PVC deletion in `isVMOwnedPVCDeleteDenied` when `spec.vms` is non-empty, not only when `status.ownership=VMManaged` — `webhooks/persistentvolumeclaim/validation/persistentvolumeclaim_validator.go`
- [x] T075 [vmop-TBD] Unit tests for all five items above — `webhooks/virtualmachine/validation/virtualmachine_validator_unit_test.go`, `webhooks/persistentvolumeclaim/validation/persistentvolumeclaim_validator_unit_test.go`

## Phase 9 — Report VM-owned volume status for every disk mode

Dependencies: Phase 2. Independent of Phase 7/8 once Phase 3 has landed.

- [x] T080 [vmop-TBD] Verify (no code change needed): the dependent-mode attach path already writes `Type: Managed` and slot fields at attach time, not a later observation pass — traced in `attachReadyDisks` — `controllers/virtualmachine/volumeattachdetach/volumeattachdetach_ownedvolumes.go`
- [x] T081 [vmop-TBD] Trace the independent-mode status gap: an independent disk is still an FCD, and `updateVolumeStatus`'s generic scan never creates a fresh entry for an FCD it does not already know about. No fix needed today (independent device attach is deferred — Non-goals), but leave a comment at the exact spot so the obligation to write `Type: Managed` directly isn't missed when that lands — `controllers/virtualmachine/volumeattachdetach/volumeattachdetach_ownedvolumes.go`
- [x] T082 [vmop-TBD] Add a test for the registration-pass provenance discriminator against the shape migration will produce in bulk: a clean-migrated dependent disk (plain VMDK, real bound PVC, no placeholder `dataSourceRef`) must not be re-registered as unmanaged — `pkg/vmconfig/volumes/unmanaged/register/unmanagedvolumes_register_test.go`

## Phase 10 — Wire RBAC, docs, SDD artifacts

Dependencies: all prior Pass 1 phases.

- [x] T090 [vmop-TBD] Feature-gate audit: confirm every branch from Phases 2–9 is behind `Features.VMOwnedVolumes`, and confirm which branches must additionally check the per-VM annotation (attach/detach control-flow decisions) versus which must not (registration-pass and status-typing code that must also treat a mid-migration, not-yet-annotated VM as VM-owned by disk shape) — no code change; findings recorded in `plan.md`'s Controller/webhook impact section
- [x] T091 [vmop-TBD] Regenerate `config/rbac/role.yaml` via `controller-gen ... rbac:roleName=manager-role` — `config/rbac/role.yaml`
- [x] T092 [vmop-TBD] Confirm no vm-operator code path writes `cns.vmware.com/usedby-vm-<uuid>` (CSI's alone, csi.md §13.8); add a comment at the CVI `spec.vms` patch site where someone would be tempted to also stamp it — `controllers/virtualmachine/volumeattachdetach/volumeattachdetach_ownedvolumes.go`
- [x] T093 [vmop-TBD] Rewrite `spec.md`, `plan.md`, `tasks.md` against the finalized, all-disk-modes design; register this spec in `.sdd/INDEX.md` — `.sdd/specs/002-vm-owned-volumes/spec.md`, `.sdd/specs/002-vm-owned-volumes/plan.md`, `.sdd/specs/002-vm-owned-volumes/tasks.md`, `.sdd/INDEX.md`
- [x] T094 [vmop-TBD] Add `model.md` for the CVI surface (data model changed this pass — `SHOULD` per the constitution) — `.sdd/specs/002-vm-owned-volumes/model.md`
- [ ] T095 [vmop-TBD] Docs: feature gate, VM annotation semantics, and the pre-disable-gate detach-everything requirement — `docs/`

---

## Phase Final — Polish (blocked on ticket filing)

- [ ] T100 [vmop-TBD] File the JIRA epic; replace every `vmop-TBD` in this spec's artifacts with the real `vmop-NNN`; file a story/sub-task per phase above and set `customfield_10830` — `.sdd/specs/002-vm-owned-volumes/spec.md`, `.sdd/specs/002-vm-owned-volumes/plan.md`, `.sdd/specs/002-vm-owned-volumes/tasks.md`

---

## Out of scope for this spec (tracked separately, Pass 2)

Not tasks of this spec — listed here only so a reader does not mistake their absence for an oversight. See `cns-specs/VGL-62908/implementation/vmop.md` §15–16.

- Migrate brownfield VMs onto the CsiVolumeInfo path (V11): BA freeze → per-disk migrate → BA retire, annotation flip.
- Convert VKS node disks to independent-persistent (V12).
- Independent-mode device attach (this spec's Non-goals) — needed by both V12 and any future independent-mode attach outside migration.
