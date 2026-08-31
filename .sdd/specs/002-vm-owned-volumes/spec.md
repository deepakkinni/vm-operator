# Feature Specification: VM-Owned Volume Attach/Detach

- **Feature branch**: `topic/dk016388/vmown-impl-v2`
  - **Fork**: `vmware-tanzu/vm-operator`
  - **PR target**: `vmware-tanzu/vm-operator`
- **Created**: 2026-06-23
- **Updated**: 2026-08-07
- **Status**: Implemented (attach/detach; migration is a separate follow-on spec)
- **Epic**: vmop-TBD
- **Design docs**: See `cns-specs/VGL-62908/vm-owned-volume-attach-detach-spec.md` for the full cross-component specification, and `cns-specs/VGL-62908/implementation/vmop.md` for the vm-operator-specific implementation plan this spec tracks.

---

## Summary

When the `VMOwnedVolumes` feature gate is enabled, a VM-owned-volumes VM (marked by the immutable annotation `vmoperator.vmware.com/vm-owned-volumes: "true"`) coordinates **every** PVC-backed disk — in every disk mode — through a per-volume CR, `CsiVolumeInfo` (CVI), instead of the legacy `CnsNodeVMBatchAttachment` (BA). vm-operator attaches and detaches these disks itself via raw `ReconfigVM_Task` calls; the BA plays no role at all for such a VM, regardless of disk mode.

A **dependent** disk (`Persistent` mode, including the plain-VMDK-with-a-real-bound-PVC shape, `fcd-retained`) transfers FCD ownership to the VM: CSI unregisters the FCD and the disk becomes a plain VMDK owned directly by the VM. An **independent** disk (`IndependentPersistent`, `IndependentNonPersistent`, `NonPersistent`) keeps the FCD registered and `CSIManaged` forever — only the CVI entry changes.

This spec covers **vm-operator's responsibilities only**. The full end-to-end data model, CSI's side of the contract, and the migration interface are in `cns-specs/VGL-62908/vm-owned-volume-attach-detach-spec.md`.

---

## Goals

- vm-operator MUST stamp the annotation `vmoperator.vmware.com/vm-owned-volumes: "true"` on every new `VirtualMachine` created while the `VMOwnedVolumes` feature gate is enabled. The annotation is immutable once set: only the absent → `"true"` transition is permitted; any other change is rejected by the validating webhook, non-admin or admin, regardless of principal (the transition is scoped, not principal-scoped, since a future migration controller performs the same transition on an existing VM).
- vm-operator MUST route **every** PVC-backed disk on a VM-owned-volumes VM through the CsiVolumeInfo path, in every disk mode. Such a VM MUST NOT receive a `CnsNodeVMBatchAttachment`; the batch path is suppressed entirely for it.
- vm-operator MUST follow the attach workflow (Workflow A) for a VM-owned-volumes VM: resolve or create the CVI for the PVC, append/update `{vmName, vmInstanceUUID, diskMode, volumeName}` in `CsiVolumeInfo.spec.vms`, and then:
  - **Dependent mode**: wait for the green signal (`status.ownership=VMManaged`, `status.observedGeneration >= metadata.generation`, `status.phase=Succeeded`, independent of the `fcd-retained` annotation) and execute one batched `ReconfigVM_Task` to add every ready disk as a plain VMDK, honoring per-disk FCD attach obligations (`vDiskId` when `fcd-retained`, per-disk `changeTrackingEnabled` — never the VM-level flag) and writing `vm.status.volumes` (`type=Managed`, disk slot fields) from the attach itself, not a later observation pass.
  - **Independent mode**: the CVI entry is written; the device add is a documented, deferred follow-on (it depends on functionality this codebase does not yet have — see Non-goals).
- `CsiVolumeInfo.spec.vms[*].volumeName` (mirroring `vm.spec.volumes[*].name`) is the correlation key detach uses to pair a CVI entry to its `vm.status.volumes` entry — and therefore to its device slot — after the volume has already left `vm.spec.volumes`. vm-operator MUST write it at attach and MUST NOT key detach pairing on `diskUUID`, which is empty for an `fcd-retained` disk and ambiguous when more than one volume is removed in a single edit.
- vm-operator MUST follow the detach workflow (Workflow B) for a VM-owned-volumes VM: for a CVI entry whose volume has left `vm.spec.volumes`, resolve its device slot via `volumeName`, refresh `CsiVolumeInfo.spec.diskPath` just-in-time from the live VM device (dependent mode only), execute `ReconfigVM_Task` to remove the disk (preserving the VMDK file), and remove the VM's entry from `CsiVolumeInfo.spec.vms` (immediately for independent mode; only once the CVI is not snapshot-retained for dependent mode).
- A volume MUST attach in exactly one disk mode across every VM it is attached to; vm-operator MUST reject an attach that would create a second, differently-moded relationship for the same PVC. Within a single disk mode, an RWM volume MAY be attached to multiple VMs simultaneously; an RWO volume MUST NOT be attached to a second VM while a CVI entry for a different VM already exists.
- vm-operator MUST capture disk paths and evaluate CVI entry removal during snapshot deletion (Workflow D) and during VM revert (Workflow E) using the two-tier check: fast path via the `VirtualMachineSnapshot` CR's frozen `pvc.disk.data`, authoritative backstop via a live vCenter snapshot-tree query (required to cover unmanaged snapshots that carry no CR).
- vm-operator MUST reject a snapshot revert whose target snapshot has a non-empty `metadata.deletionTimestamp` — reverting to a snapshot mid-deletion races the delete's own disk-deregistration cleanup.
- vm-operator MUST block PVC deletion whenever the bound `CsiVolumeInfo` has a non-empty `spec.vms` (an attached relationship in any mode), not only when `status.ownership=VMManaged` — an independent-mode volume never transitions to `VMManaged`, so ownership alone misses it.
- vm-operator MUST run a periodic `CsiVolumeInfo` sweeper that removes a `spec.vms` entry whose VM no longer exists, confirmed via a live (uncached) read and re-confirmed after a grace period, so a VM merely absent from cache is never mistaken for deleted.
- All new behavior MUST be gated by the `VMOwnedVolumes` feature gate; when the gate is off, vm-operator MUST fall through to the existing legacy `CnsNodeVMBatchAttachment` / `CnsAttachVolume` / `CnsDetachVolume` path unchanged. A VM lacking the annotation (brownfield, not yet migrated) MUST also use the legacy path unchanged even when the gate is on.
- vm-operator MUST NOT write the `cns.vmware.com/usedby-vm-<uuid>` label on any PVC — that bookkeeping belongs to CSI alone, keyed on the same `spec.vms` becoming non-empty.

---

## Non-goals

- **Brownfield VM migration** (converting an existing, unannotated VM onto the CsiVolumeInfo path, including the VKS disk-mode conversion). This is a separate, second pass (`cns-specs/VGL-62908/implementation/vmop.md` §15–16) tracked by its own follow-on spec once this spec's attach/detach behavior has been exercised end to end.
- **Independent-mode device attach.** The CVI entry for an independent-mode disk is written correctly, but the `ReconfigVM_Task` that would add the device (and the per-disk CBT declaration it requires, §7.1.3 of the full spec) depends on a CNS/vslm client this codebase does not have, and no verified mechanism exists yet for resolving the needed identifiers from a `PersistentVolume`. This is a deliberate, documented gap, not an oversight — see `pkg/providers/vsphere/vmprovider_vm_ownedvolumes.go` and `controllers/virtualmachine/volumeattachdetach/volumeattachdetach_ownedvolumes.go`.
- Volume expansion for VM-owned volumes (separate spec).
- VM import with VM-owned volumes (separate spec).
- Encryption interactions (separate spec).
- VM deletion with `Retain` reclaim policy handling (separate spec).
- File-backed (non-block) volume support.
- Cross-vCenter volume operations.
- Feature gate rollback while volumes are VM-managed or attached in any mode. Operators must detach every VM-owned volume before disabling the gate; rollback is not supported.

---

## User stories / acceptance criteria

### US1 — DevOps user: VM-owned VM attach, dependent mode (Priority: P1)

**Given** a VM carries the `vmoperator.vmware.com/vm-owned-volumes` annotation and a PVC is added to `vm.spec.volumes` in dependent (`Persistent`) mode, **when** vm-operator reconciles, **then** the PVC's `CsiVolumeInfo.spec.vms` contains an entry for the VM (with `diskMode` and `volumeName` set), the FCD is unregistered by CSI (`status.ownership=VMManaged`), and the disk is added to the VM via a batched `ReconfigVM_Task` as a plain VMDK. The PVC is reflected in `vm.status.volumes` with `type=Managed` and its device slot fields populated.

**Acceptance scenarios:**

1. **Given** a new VM-owned VM exists and a PVC has a `CsiVolumeInfo` with `status.ownership=CSIManaged` and empty `spec.vms`, **when** the user adds the PVC to `vm.spec.volumes`, **then** vm-operator resolves or creates the CVI, appends `{vmName, vmInstanceUUID, diskMode, volumeName}` to `spec.vms`, CSI transitions ownership to `VMManaged`, and vm-operator executes `ReconfigVM_Task` to add the plain VMDK.
2. **Given** the same RWM PVC is already attached to one VM-owned VM in `Persistent` mode, **when** a second VM-owned VM adds it in the same mode, **then** vm-operator appends the second VM's entry, CSI stays idle, and vm-operator immediately executes `ReconfigVM_Task` on the second VM.
3. **Given** a PVC's `CsiVolumeInfo.spec.vms` already contains an entry for a different VM in a different disk mode, **when** a VM attempts to attach it, **then** the VM validating webhook rejects the attach: "PVC is already attached to VM %q in a different disk mode."
4. **Given** a RWO PVC's `CsiVolumeInfo.spec.vms` already contains a different VM (any mode), **when** a second VM attempts to attach it, **then** the webhook rejects the attach: "PVC is already attached to another VM."
5. **Given** vm-operator crashes after appending the entry but before `ReconfigVM_Task`, **when** vm-operator restarts, **then** it observes the green signal and retries `ReconfigVM_Task` idempotently, batching it with any other disk that became ready in the interim.
6. **Given** an `fcd-retained` volume (the FCD could not be unregistered), **when** vm-operator attaches it, **then** the attach still proceeds using the observed `vDiskId` so the platform's linked-clone precheck has a valid FCD identity, and no VM-level `changeTrackingEnabled` is ever set on the reconfigure.

### US2 — DevOps user: VM-owned VM detach (Priority: P1)

**Given** a VM-owned VM and a PVC is removed from `vm.spec.volumes`, **when** vm-operator reconciles, **then** the CVI entry is resolved by `volumeName` against `vm.status.volumes` to find the device slot, `CsiVolumeInfo.spec.diskPath` is refreshed from the live VM device (dependent mode), the disk is removed from the VM via `ReconfigVM_Task` (VMDK file preserved), the VM's entry is removed from `CsiVolumeInfo.spec.vms`, and — for dependent mode, once no snapshot retains the disk — CSI re-registers the FCD.

**Acceptance scenarios:**

1. **Given** a dependent-mode PVC is the sole attachment (`spec.vms` has one entry) and no snapshot retains the disk, **when** the user removes it, **then** `spec.diskPath` is refreshed, `ReconfigVM_Task` removes the disk, the entry is removed, and CSI auto-registers the FCD (`status.ownership=CSIManaged`).
2. **Given** an independent-mode PVC is removed, **when** vm-operator reconciles, **then** the VM's entry is removed from `spec.vms` immediately — no `ReconfigVM_Task` and no `diskPath` refresh, since the disk was never added as a device by vm-operator.
3. **Given** a vSphere snapshot blocks disk removal, **when** `ReconfigVM_Task` fails, **then** the error is surfaced, the entry stays in `spec.vms`, and vm-operator retries.
4. **Given** a PVC was removed from `vm.spec.volumes` but the disk is already off the VM (mid-workflow crash), **when** vm-operator reconciles, **then** it removes the entry from `spec.vms` without issuing another `ReconfigVM_Task`.
5. **Given** two volumes are removed from `vm.spec.volumes` in the same edit, **when** vm-operator reconciles, **then** each CVI entry is paired to the correct device slot via its own `volumeName`, never by `diskUUID` (empty for `fcd-retained`, ambiguous across the two removed volumes).

### US3 — DevOps user: snapshot deletion cleans up CVI entries (Priority: P2)

**Given** a `VirtualMachineSnapshot` is deleted that retained VM-owned volumes, **when** vm-operator processes the deletion, **then** disk paths are captured from the snapshot's device config (base-walked to the root VMDK) before the vSphere snapshot is deleted, the snapshot is deleted, and CVI entries are removed for disks that are no longer on the VM and no longer retained by any snapshot (including unmanaged snapshots verified via a live vCenter snapshot-tree query).

**Acceptance scenarios:**

1. **Given** snapshot-2 retains `pvc-d3` but snapshot-1 also retains it, **when** snapshot-2 is deleted, **then** the CVI entry for `pvc-d3` is preserved.
2. **Given** snapshot-2 is the only snapshot retaining `pvc-d3` and the disk is not on the VM, **when** snapshot-2 is deleted, **then** the CVI entry is removed and CSI re-registers the FCD.
3. **Given** an unmanaged snapshot (no `VirtualMachineSnapshot` CR) retains `pvc-d3`, **when** any managed snapshot is deleted, **then** the live vCenter snapshot-tree query is consulted and the entry is preserved.

### US4 — DevOps user: revert evaluates dropped volumes (Priority: P2)

**Given** a user reverts a VM to a snapshot, **when** vm-operator processes the revert, **then** disk paths for volumes that will be dropped are captured from the live VM device before the revert, the revert executes, and CVI entries are removed for disks that are no longer on the VM and not retained by any snapshot. Disks that were present at snapshot time are re-adopted without an ownership change.

**Acceptance scenarios:**

1. **Given** a revert drops `pvc-d3` (added after the snapshot) and no snapshot retains it, **when** the revert completes, **then** the CVI entry for `pvc-d3` is removed and CSI re-registers the FCD.
2. **Given** a revert drops `pvc-d3` but snapshot-2 retains it, **when** the revert completes, **then** the CVI entry stays.
3. **Given** the target `VirtualMachineSnapshot` has a non-empty `metadata.deletionTimestamp`, **when** the user sets `vm.spec.currentSnapshotName`, **then** the validating webhook rejects the revert: "cannot revert to a snapshot that is being deleted."
4. **Given** a revert is already in progress (`oldVM.spec.currentSnapshotName` non-empty and different from the new value), **when** the user changes `currentSnapshotName` again, **then** the webhook rejects it, and the revert-target-deletion check above does not run (it applies only to a newly-requested revert).

### US5 — DevOps user: PVC deletion blocked while attached (Priority: P1)

**Given** a PVC whose `CsiVolumeInfo` has `status.ownership=VMManaged`, or has a non-empty `spec.vms` in any mode, **when** a user or process attempts to delete the PVC, **then** the admission webhook rejects the deletion with a descriptive error message.

**Acceptance scenarios:**

1. **Given** `CsiVolumeInfo.status.ownership=VMManaged`, **when** `kubectl delete pvc <name>` is run, **then** admission is denied: "volume is VM-managed; detach from all VMs and delete retaining snapshots first."
2. **Given** `CsiVolumeInfo.status.ownership=CSIManaged` but `spec.vms` is non-empty (an attached independent-mode volume), **when** `kubectl delete pvc <name>` is run, **then** admission is denied: "volume is attached to a VM-owned VM; detach it first."
3. **Given** `CsiVolumeInfo.status.ownership=CSIManaged` and `spec.vms` is empty, **when** `kubectl delete pvc <name>` is run, **then** admission is allowed.
4. **Given** the webhook is unavailable, **then** the `csi.vsphere.vmware.com/volume-protection` finalizer (VMManaged) and `csi.vsphere.vmware.com/pvc-volume-protection` finalizer (attached independent volume) — both written by CSI — prevent the PVC from being garbage-collected.

### US6 — Platform engineer: feature gate controls the new path (Priority: P1)

**Given** the `VMOwnedVolumes` feature gate is disabled, **when** a VM is created or a PVC is attached, **then** all behavior is identical to the pre-feature state: the VM annotation is not stamped, and attach/detach goes through the legacy `CnsNodeVMBatchAttachment` path.

**Acceptance scenarios:**

1. **Given** the feature gate is off, **when** a new VM is created, **then** the `vmoperator.vmware.com/vm-owned-volumes` annotation is absent.
2. **Given** the feature gate is on but a VM lacks the annotation (brownfield, not yet migrated), **when** a PVC is attached, **then** the legacy path runs unchanged, in every disk mode.
3. **Given** the feature gate is turned on after being off, **when** new VMs are created, **then** they receive the annotation and subsequent attaches follow the new path.
4. **Given** a VM carries the annotation, **when** any principal (admin or not) attempts to change or remove it, **then** the validating webhook rejects the change — the transition is immutable once set, not principal-gated.

---

## Edge cases

- A volume with a concurrent in-flight attach race (between the CVI entry being written and CSI reaching `VMManaged`) is caught by the VM webhook's `spec.vms` check, which is ownership-independent and applies to every disk mode, not only dependent.
- A missing `CsiVolumeInfo` on a VM-owned VM at attach time is an anomaly to repair, not a brownfield PVC to skip: vm-operator creates it (owned by the `PersistentVolume`) rather than treating the PVC as ineligible.
- Unmanaged snapshots (no `VirtualMachineSnapshot` CR) are invisible at the Kubernetes layer; the vCenter snapshot-tree query in Workflow D / E is the authoritative backstop and is mandatory, not best-effort.
- `diskPath` may be stale due to storage vMotion; the just-in-time resolution strategy (capture from the live VM device, or from the snapshot device config immediately before each consumption point) keeps the path current at the moment it is needed. Base-walking to the root VMDK is correct for snapshot-context resolution; the device's own current backing path is correct for a live-VM detach refresh — these must not be conflated.
- A registration-pass ambiguity: a clean-migrated (or otherwise already-transferred) dependent disk is, by hardware shape alone, a non-FCD disk with a PVC-backed `spec.volumes` entry — indistinguishable from an unmanaged classic disk awaiting a registration placeholder. The provenance discriminator (whether the bound PVC is this VM's own registration placeholder, identified by `dataSourceRef`) is what tells them apart, and must never be confused with a state-based check (bound vs. pending).
- A CVI entry whose VM has been deleted without going through the normal detach path is caught by the periodic sweeper, not treated as a permanent orphan.

---

## Open questions

- [NEEDS CLARIFICATION: Independent-mode device attach needs a resolved mechanism for obtaining the vSphere `vDiskId`/CBT-declaration inputs from a `PersistentVolume` alone — no verified CNS/vslm client or PV field exists in this codebase today. Blocks the non-goal above from becoming in-scope.]
- [NEEDS CLARIFICATION: Out-of-band deletion of a managed vSphere snapshot (via vCenter UI) leaves a stale `VirtualMachineSnapshot` CR held by the vm-operator finalizer. Recovery mechanism for stale CRs is TBD (open question #1 in the full spec).]
- [NEEDS CLARIFICATION: The exact govmomi traversal for the authoritative disk-chain query against the vCenter snapshot tree (Workflow D.4 / E.5) should be reviewed against the current implementation for completeness as new snapshot topologies are exercised.]
