# VM-Owned Volumes

By default, VM Operator attaches a `PersistentVolumeClaim` (PVC) to a `VirtualMachine` (VM) as an FCD (First Class Disk), managed jointly with the CSI driver via a `CnsNodeVMBatchAttachment`. When the `VMOwnedVolumes` feature gate is enabled, a VM instead coordinates every PVC-backed disk directly with CSI through a per-volume `CsiVolumeInfo` resource, and VM Operator itself attaches and detaches the disk with the VM's `ReconfigVM_Task`.

## Feature gate

`VMOwnedVolumes` (`FSS_WCP_VM_OWNED_VOLUMES`) is off by default. A platform operator enables it cluster-wide. Only VMs **created after** the gate is enabled use this path — existing VMs are unaffected until a future migration (not yet available) converts them.

## The `vm-owned-volumes` annotation

When the gate is enabled, VM Operator stamps `vmoperator.vmware.com/vm-owned-volumes: "true"` on every new VM at creation. This annotation is:

- **Immutable once set.** No user or process may change or remove it after creation.
- **The switch that routes the VM's volumes.** A VM without the annotation always uses the legacy `CnsNodeVMBatchAttachment` path, even while the feature gate is on.

## Disk modes

Every PVC-backed disk on a VM-owned-volumes VM is coordinated through the CVI, regardless of disk mode:

- **Dependent (`Persistent`)**: CSI unregisters the underlying FCD and the disk becomes a plain VMDK owned by the VM.
- **Independent (`IndependentPersistent`, `IndependentNonPersistent`, `NonPersistent`)**: the FCD stays registered with CSI; only the VM's relationship to the volume is recorded.

A given PVC always attaches in exactly one disk mode across every VM that uses it. VM Operator rejects an attach that would create a second, differently-moded relationship for the same PVC.

## What an operator must do before disabling the gate

**The feature gate cannot be safely disabled while any VM-owned volume exists, in any mode.** There is no rollback path: turning the gate off does not convert VM-owned volumes back to the legacy path, and a VM's disks left in this state after the gate is disabled will not reconcile correctly.

Before disabling `VMOwnedVolumes`:

1. Detach every PVC from every VM-owned-volumes VM (remove it from `vm.spec.volumes` and wait for the disk to leave `vm.status.volumes`).
2. Confirm no `CsiVolumeInfo` resource has a non-empty `spec.vms` for a VM you intend to keep.
3. Only then disable the feature gate.

## Related

- [`VirtualMachine`](./vm.md)
- [`VirtualMachineSnapshot`](./vm-snapshot.md) — snapshot delete and revert both evaluate and clean up VM-owned volume state.
