# Feature Specification: VM-Owned Volume Attach/Detach

- **Feature branch**: `feature/vm-owned-volumes`
  - **Fork**: `vmware-tanzu/vm-operator`
  - **PR target**: `vmware-tanzu/vm-operator`
- **Created**: 2026-06-23
- **Status**: Draft
- **Epic**: vmop-TBD
- **Design docs**: See `cns-specs/VGL-62908/vm-owned-volume-attach-detach-spec.md` for the full specification.

---

## Summary

When the `VMOwnedVolumes` feature gate is enabled, a new class of VM (a **greenfield VM**) attaches PersistentVolumeClaims in dependent-persistent mode by unregistering the underlying FCD and adding the disk as a plain VMDK owned directly by the VM. This eliminates the mixed-snapshot-chain problem by ensuring only VM snapshots (not CSI/CNS snapshots) act on attached disks. A new per-volume CR `CsiVolumeInfo` (written by CSI, read and partially written by vm-operator) coordinates the ownership transfer between vm-operator and the CSI driver.

This spec covers **vm-operator's responsibilities only**. The full end-to-end data model, CSI side of the contract, and migration interface are in `cns-specs/VGL-62908/vm-owned-volume-attach-detach-spec.md`.

---

## Goals

- vm-operator MUST stamp the annotation `vmoperator.vmware.com/vm-owned-volumes: "true"` on every new `VirtualMachine` created while the `VMOwnedVolumes` feature gate is enabled. The annotation is immutable after creation.
- vm-operator MUST follow the greenfield attach path (Workflow A) for any VM carrying the annotation: append an entry for the VM to `CsiVolumeInfo.spec.vms`, wait for the green signal (`status.ownership=VMManaged`, `status.observedGeneration >= metadata.generation`, `status.phase=Succeeded`), and then execute `ReconfigVM_Task` to add the disk as a plain VMDK.
- vm-operator MUST follow the greenfield detach path (Workflow B) for any VM carrying the annotation: refresh `CsiVolumeInfo.spec.diskPath` just-in-time from the live VM device, execute `ReconfigVM_Task` to remove the disk (preserving the VMDK file), and then remove the VM's entry from `CsiVolumeInfo.spec.vms`.
- vm-operator MUST capture diskPaths and evaluate entry removal during snapshot deletion (Workflow D) and during VM revert (Workflow E) using the two-tier check: fast path via VMSnap CR `pvc.disk.data`, authoritative backstop via live vCenter snapshot-tree query (required to cover unmanaged snapshots).
- vm-operator MUST place the `vmoperator.vmware.com/cvi-cleanup` finalizer on every greenfield VM and drive any in-flight CVI entries to completion on VM deletion.
- vm-operator MUST reject attach requests for RWO volumes whose `CsiVolumeInfo.spec.vms` already contains a different VM (concurrent-attach protection).
- vm-operator MUST block PVC deletion when `CsiVolumeInfo.status.ownership=VMManaged` via a ValidatingAdmissionWebhook on PVC DELETE.
- vm-operator SHOULD surface attach/detach errors clearly on `vm.status.volumes` with structured conditions.
- vm-operator SHOULD implement a periodic CVI sweeper that removes stale `spec.vms` entries whose `vmInstanceUUID` no longer resolves to an existing VM CR.
- All new behavior MUST be gated by the `VMOwnedVolumes` feature gate; when the gate is off, vm-operator MUST fall through to the existing legacy `CnsNodeVMBatchAttachment` / `CnsAttachVolume` / `CnsDetachVolume` path unchanged.
- Brownfield VMs (annotation absent) MUST use the legacy path unchanged, regardless of whether the feature gate is on.

---

## Non-goals

- Brownfield VM migration (converting existing VMs to the greenfield path). Section 15 of the full spec defines the high-level interface contract; full implementation is a future spec.
- Volume expansion for VM-owned volumes (separate spec).
- VM import with VM-owned volumes (separate spec).
- Encryption interactions (separate spec).
- VM deletion with `Retain` reclaim policy handling (separate spec).
- File-backed (non-block) volume support.
- Cross-vCenter volume operations.
- Feature gate rollback while volumes are VM-managed. Operators must detach all VM-owned volumes before disabling the gate.
- Independent-mode RWM volumes (they remain as registered FCDs via the legacy path).

---

## User stories / acceptance criteria

### US1 — DevOps user: greenfield VM attach (Priority: P1)

**Given** a VM is greenfield (annotation `vmoperator.vmware.com/vm-owned-volumes: "true"`) and a PVC is added to `vm.spec.volumes` in dependent-persistent mode, **when** vm-operator reconciles, **then** the PVC's `CsiVolumeInfo.spec.vms` contains an entry for the VM, the FCD is unregistered by CSI (`status.ownership=VMManaged`), and the disk is added to the VM via `ReconfigVM_Task` as a plain VMDK. The PVC is reflected in `vm.status.volumes` with `type=Managed`.

**Acceptance scenarios:**

1. **Given** a new greenfield VM exists and a PVC has a `CsiVolumeInfo` with `status.ownership=CSIManaged` and empty `spec.vms`, **when** the user adds the PVC to `vm.spec.volumes`, **then** vm-operator appends `{vmName, vmInstanceUUID}` to `CsiVolumeInfo.spec.vms`, CSI transitions ownership to `VMManaged`, and vm-operator executes `ReconfigVM_Task` to add the plain VMDK.
2. **Given** the same RWM PVC is already attached to one greenfield VM (`status.ownership=VMManaged`), **when** a second greenfield VM adds it, **then** vm-operator appends the second VM's entry to `spec.vms`, CSI stays idle, and vm-operator immediately executes `ReconfigVM_Task` on the second VM.
3. **Given** a RWO PVC's `CsiVolumeInfo.spec.vms` already contains a different VM, **when** a second VM attempts to attach it, **then** the VM webhook rejects the attach with a descriptive error.
4. **Given** vm-operator crashes after appending the entry but before `ReconfigVM_Task`, **when** vm-operator restarts, **then** it observes the green signal and retries `ReconfigVM_Task` idempotently.

### US2 — DevOps user: greenfield VM detach (Priority: P1)

**Given** a VM is greenfield and a PVC is removed from `vm.spec.volumes`, **when** vm-operator reconciles, **then** `CsiVolumeInfo.spec.diskPath` is refreshed from the live VM device, the disk is removed from the VM via `ReconfigVM_Task` (VMDK file preserved), the VM's entry is removed from `CsiVolumeInfo.spec.vms`, and if `spec.vms` is empty CSI re-registers the FCD.

**Acceptance scenarios:**

1. **Given** a PVC is the sole attachment (`spec.vms` has one entry), **when** the user removes it, **then** `spec.diskPath` is refreshed, `ReconfigVM_Task` removes the disk, the entry is removed, and CSI auto-registers the FCD (`status.ownership=CSIManaged`).
2. **Given** a vSphere snapshot blocks disk removal, **when** `ReconfigVM_Task` fails, **then** the error is surfaced on `vm.status.volumes`, the entry stays in `spec.vms`, and vm-operator retries with exponential backoff.
3. **Given** a PVC was removed from `vm.spec.volumes` but the disk is already off the VM (mid-workflow crash), **when** vm-operator reconciles, **then** it removes the entry from `spec.vms` without issuing another `ReconfigVM_Task`.

### US3 — DevOps user: snapshot deletion cleans up CVI entries (Priority: P2)

**Given** a `VirtualMachineSnapshot` is deleted that retained vm-owned volumes, **when** vm-operator processes the deletion, **then** diskPaths are captured from the snapshot's device config before deletion, the vSphere snapshot is deleted, and CVI entries are removed for disks that are no longer on the VM and no longer retained by any snapshot (including unmanaged snapshots verified via a live vCenter snapshot-tree query).

**Acceptance scenarios:**

1. **Given** snapshot-2 retains `pvc-d3` but snapshot-1 also retains it, **when** snapshot-2 is deleted, **then** the CVI entry for `pvc-d3` is preserved (snapshot-1 still retains it).
2. **Given** snapshot-2 is the only snapshot retaining `pvc-d3` and the disk is not on the VM, **when** snapshot-2 is deleted, **then** the CVI entry is removed and CSI re-registers the FCD.
3. **Given** an unmanaged snapshot (no VMSnap CR) retains `pvc-d3`, **when** any managed snapshot is deleted, **then** the live vCenter snapshot-tree query is consulted and the entry is preserved.

### US4 — DevOps user: revert evaluates dropped volumes (Priority: P2)

**Given** a user reverts a VM to a snapshot, **when** vm-operator processes the revert, **then** diskPaths for volumes that will be dropped are captured before the revert, the revert executes, and CVI entries are removed for disks that are no longer on the VM and not retained by any snapshot. Disks that were present at snapshot time are re-adopted without ownership change.

**Acceptance scenarios:**

1. **Given** a revert drops `pvc-d3` (added after the snapshot) and no snapshot retains it, **when** the revert completes, **then** the CVI entry for `pvc-d3` is removed and CSI re-registers the FCD.
2. **Given** a revert drops `pvc-d3` but snapshot-2 retains it, **when** the revert completes, **then** the CVI entry stays (snapshot-2 still retains it).
3. **Given** the target VMSnap has a non-empty `metadata.deletionTimestamp`, **when** the user sets `vm.spec.currentSnapshotName`, **then** the webhook rejects the revert.

### US5 — DevOps user: PVC deletion blocked while VM-managed (Priority: P1)

**Given** a PVC whose `CsiVolumeInfo` has `status.ownership=VMManaged`, **when** a user or process attempts to delete the PVC, **then** the admission webhook rejects the deletion with a descriptive error message.

**Acceptance scenarios:**

1. **Given** `CsiVolumeInfo.status.ownership=VMManaged`, **when** `kubectl delete pvc <name>` is run, **then** admission is denied with an error citing that the volume is VM-managed and the user must detach first.
2. **Given** the webhook is unavailable, **then** the `csi.vsphere.vmware.com/volume-protection` finalizer on CVI (with `blockOwnerDeletion=true`) prevents the PV from being garbage-collected.

### US6 — Platform engineer: feature gate controls the new path (Priority: P1)

**Given** the `VMOwnedVolumes` feature gate is disabled, **when** a VM is created or a PVC is attached, **then** all behavior is identical to the pre-feature state: the VM annotation is not stamped, and attach/detach goes through the legacy `CnsNodeVMBatchAttachment` path.

**Acceptance scenarios:**

1. **Given** the feature gate is off, **when** a new VM is created, **then** the `vmoperator.vmware.com/vm-owned-volumes` annotation is absent.
2. **Given** the feature gate is on but a VM lacks the annotation (brownfield), **when** a PVC is attached, **then** the legacy path runs unchanged.
3. **Given** the feature gate is turned on after being off, **when** new VMs are created, **then** they receive the annotation and subsequent attaches follow the new path.

---

## Edge cases

- A RWO volume with a concurrent in-flight attach race (between A.2 and A.4, ownership still `CSIManaged` but the entry is already in `spec.vms`) is caught by the VM webhook's `spec.vms` check, which is ownership-independent.
- A VM that is deleted while a CVI entry is in-flight is handled by the `vmoperator.vmware.com/cvi-cleanup` finalizer; vm-operator drives detach to completion before releasing the finalizer.
- Brownfield PVCs (no CVI at greenfield attach time) cause vm-operator to create the CVI lazily with `spec.vms` populated; CSI's reconciler validates prerequisites before proceeding with Unregister.
- Unmanaged snapshots (no VMSnap CR) are invisible at the Kubernetes layer; the vCenter snapshot-tree query in D.4 / E.5 is the authoritative backstop and is mandatory.
- `diskPath` may be stale due to storage vMotion; the JIT resolution strategy (capture from live VM device or snapshot device config immediately before each consumption point) ensures the path is always current at the moment it is needed.

---

## Open questions

- [NEEDS CLARIFICATION: Out-of-band deletion of a managed vSphere snapshot (via vCenter UI) leaves a stale VMSnap CR held by the vm-operator finalizer. Recovery mechanism for stale VMSnap CRs is TBD (open question #1 in the full spec).]
- [NEEDS CLARIFICATION: The exact govmomi traversal for the authoritative disk-chain query against the vCenter snapshot tree (D.4 / E.5) needs to be specified at implementation. Also, unmanaged snapshots carry no MoRef/id in current vm-operator status — surfacing an identifier may require a vm-operator schema extension (open question #10 in the full spec).]
- [NEEDS CLARIFICATION: Backup-before-snapshot ordering must be confirmed to hold across all snapshot-trigger paths, not just the steady-state reconcile (open question #9 in the full spec).]
- [NEEDS CLARIFICATION: ReconfigVM detach failure when a snapshot blocks removal — should a dedicated condition be surfaced on CVI or VM status (open question #7 in the full spec).]
