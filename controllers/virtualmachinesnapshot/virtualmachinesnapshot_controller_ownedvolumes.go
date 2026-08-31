// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package virtualmachinesnapshot

import (
	apierrorsutil "k8s.io/apimachinery/pkg/util/errors"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha6"
	backupapi "github.com/vmware-tanzu/vm-operator/pkg/backup/api"
	pkgctx "github.com/vmware-tanzu/vm-operator/pkg/context"
	vmopv1util "github.com/vmware-tanzu/vm-operator/pkg/util/vmopv1"
)

// refreshCVIDiskPathsFromSnapshot resolves the base VMDK path for each
// PVC-backed disk from the named snapshot's device config and updates the
// corresponding CsiVolumeInfo.spec.diskPath. This must be called BEFORE the
// vSphere snapshot is deleted so the snapshot config is still readable —
// once it is deleted the snapshot's device config is gone, so this ordering
// must not change.
func (r *Reconciler) refreshCVIDiskPathsFromSnapshot(
	ctx *pkgctx.VirtualMachineSnapshotContext,
	vm *vmopv1.VirtualMachine,
	pvcDisks []backupapi.PVCDiskData) {

	vmSnapshot := ctx.VirtualMachineSnapshot

	for _, disk := range pvcDisks {
		if disk.UUID == "" {
			continue
		}

		diskPath, err := r.VMProvider.GetDiskPathFromSnapshot(
			ctx.Context, vm, vmSnapshot.Name, disk.UUID)
		if err != nil {
			ctx.Logger.V(4).Info("Could not resolve base disk path from snapshot, skipping CVI diskPath refresh",
				"pvcName", disk.PVCName, "diskUUID", disk.UUID, "error", err.Error())
			continue
		}
		if diskPath == "" {
			continue
		}

		// disk.UUID is the VirtualDisk backing UUID used above to pick the
		// right device out of the snapshot's saved config — it is not the
		// CNS volume ID, so it cannot name the CVI directly. Resolve via
		// the PVC → PV → volumeHandle chain instead, creating the CVI if a
		// VM-owned VM does not have one yet (a missing CVI here is an
		// anomaly to repair, not a brownfield PVC to skip).
		cvi, err := vmopv1util.EnsureCVIForPVC(ctx, r.Client, vmSnapshot.Namespace, disk.PVCName)
		if err != nil {
			ctx.Logger.V(4).Info("Could not resolve CsiVolumeInfo for PVC, skipping diskPath refresh",
				"pvcName", disk.PVCName, "error", err.Error())
			continue
		}

		if cvi.Spec.DiskPath == diskPath {
			continue
		}

		patch := ctrlclient.MergeFrom(cvi.DeepCopy())
		cvi.Spec.DiskPath = diskPath
		if err := r.Client.Patch(ctx, cvi, patch); err != nil {
			ctx.Logger.Error(err, "Failed to refresh CVI diskPath from snapshot",
				"pvcName", disk.PVCName, "diskPath", diskPath)
			continue
		}

		ctx.Logger.Info("Refreshed CVI diskPath from snapshot device config",
			"pvcName", disk.PVCName, "diskPath", diskPath)
	}
}

// reconcileOwnedVolumeSnapshotDeletion evaluates CsiVolumeInfo entries for
// vm-owned volumes retained by the snapshot being deleted. It removes a
// VM's entry from spec.vms for each disk that is no longer attached to the
// VM and not retained by any remaining snapshot.
//
// Called from ReconcileDelete after the vSphere snapshot has been deleted.
//
// NOTE: This covers managed snapshots only (those with a VirtualMachineSnapshot
// CR). Unmanaged snapshots (out-of-band, no CR) require a vCenter snapshot tree
// query as the authoritative backstop; that is left for a follow-up.
func (r *Reconciler) reconcileOwnedVolumeSnapshotDeletion(
	ctx *pkgctx.VirtualMachineSnapshotContext,
	pvcDisks []backupapi.PVCDiskData) error {

	vmSnapshot := ctx.VirtualMachineSnapshot
	vm := ctx.VM

	var errs []error

	for _, disk := range pvcDisks {
		if err := r.evaluateCVIForDeletedSnapshot(ctx, vm, vmSnapshot, disk); err != nil {
			errs = append(errs, err)
		}
	}

	return apierrorsutil.NewAggregate(errs)
}

// evaluateCVIForDeletedSnapshot evaluates whether the VM entry should be
// removed from the CsiVolumeInfo for a single PVC-backed disk, once the
// disk is confirmed no longer attached to the VM. The retention check
// itself, and the actual removal, are shared with Workflow E.5
// (evaluateDroppedVolumeCVIEntries) via vmopv1util.RemoveVMEntryIfNotRetained.
func (r *Reconciler) evaluateCVIForDeletedSnapshot(
	ctx *pkgctx.VirtualMachineSnapshotContext,
	vm *vmopv1.VirtualMachine,
	vmSnapshot *vmopv1.VirtualMachineSnapshot,
	disk backupapi.PVCDiskData) error {

	logger := ctx.Logger.WithValues("pvcName", disk.PVCName)

	// Check if the disk is still attached to the VM (present in spec.volumes).
	for _, vol := range vm.Spec.Volumes {
		if vol.PersistentVolumeClaim != nil &&
			vol.PersistentVolumeClaim.ClaimName == disk.PVCName {
			logger.V(4).Info("Disk is still attached to VM, keeping CVI entry")
			return nil
		}
	}

	// The deleting snapshot is excluded from the fast-path retention check.
	return vmopv1util.RemoveVMEntryIfNotRetained(
		ctx, r.Client, r.VMProvider, logger, vm, vmSnapshot.Spec.VMName, vmSnapshot.Name, disk.PVCName, disk.UUID)
}
