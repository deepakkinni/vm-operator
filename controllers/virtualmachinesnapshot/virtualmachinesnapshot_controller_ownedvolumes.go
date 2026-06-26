// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package virtualmachinesnapshot

import (
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apierrorsutil "k8s.io/apimachinery/pkg/util/errors"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha6"
	cnsv1alpha1 "github.com/vmware-tanzu/vm-operator/external/vsphere-csi-driver/api/v1alpha1"
	backupapi "github.com/vmware-tanzu/vm-operator/pkg/backup/api"
	pkgconst "github.com/vmware-tanzu/vm-operator/pkg/constants"
	pkgctx "github.com/vmware-tanzu/vm-operator/pkg/context"
	vmopv1util "github.com/vmware-tanzu/vm-operator/pkg/util/vmopv1"
)

// refreshCVIDiskPathsFromSnapshot resolves the base VMDK path for each
// PVC-backed disk from the named snapshot's device config and updates the
// corresponding CsiVolumeInfo.spec.diskPath. This must be called BEFORE the
// vSphere snapshot is deleted so the snapshot config is still readable. It
// implements spec §10.2 D.2: ensure CVI carries the registerable base-disk
// path before the snapshot is removed and CSI triggers a re-registration.
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

		// Look up the CVI directly by the disk UUID (= CNS volume ID = FCD UUID),
		// which is deterministic and avoids the PVC → PV → volumeHandle chain.
		cvi := &cnsv1alpha1.CsiVolumeInfo{}
		cviKey := ctrlclient.ObjectKey{
			Namespace: pkgconst.CVISystemNamespace,
			Name:      vmopv1util.CVINameForVolumeID(disk.UUID),
		}
		if err := r.Client.Get(ctx, cviKey, cvi); err != nil {
			ctx.Logger.V(4).Info("CVI not found for disk UUID, skipping diskPath refresh",
				"pvcName", disk.PVCName, "diskUUID", disk.UUID, "error", err.Error())
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
// removed from the CsiVolumeInfo for a single PVC-backed disk.
func (r *Reconciler) evaluateCVIForDeletedSnapshot(
	ctx *pkgctx.VirtualMachineSnapshotContext,
	vm *vmopv1.VirtualMachine,
	vmSnapshot *vmopv1.VirtualMachineSnapshot,
	disk backupapi.PVCDiskData) error {

	logger := ctx.Logger.WithValues("pvcName", disk.PVCName)

	// Look up the CsiVolumeInfo for this PVC.
	cvi, err := vmopv1util.GetCVIForPVC(ctx, r.Client, vmSnapshot.Namespace, disk.PVCName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Brownfield volume — no CVI to manage.
			logger.V(5).Info("No CVI found for PVC, skipping (brownfield volume)")
			return nil
		}
		return fmt.Errorf("failed to get CVI for PVC %q: %w", disk.PVCName, err)
	}

	// If the CVI has no VM entry for this VM, nothing to clean up.
	if !vmopv1util.HasVMEntry(cvi, vmSnapshot.Spec.VMName) {
		logger.V(5).Info("CVI has no VM entry, skipping")
		return nil
	}

	// Check if the disk is still attached to the VM (present in spec.volumes).
	for _, vol := range vm.Spec.Volumes {
		if vol.PersistentVolumeClaim != nil &&
			vol.PersistentVolumeClaim.ClaimName == disk.PVCName {
			logger.V(4).Info("Disk is still attached to VM, keeping CVI entry")
			return nil
		}
	}

	// Check if any remaining snapshot (managed or unmanaged) still retains this
	// disk. The deleting snapshot is excluded from the managed fast path.
	retained, err := vmopv1util.IsDiskRetainedBySnapshot(
		ctx, r.Client, r.VMProvider, ctx.Logger, vm, vmSnapshot.Name, disk.PVCName, disk.UUID)
	if err != nil {
		return fmt.Errorf("failed to check snapshot retention for PVC %q: %w", disk.PVCName, err)
	}
	if retained {
		logger.V(4).Info("Disk is retained by another snapshot, keeping CVI entry")
		return nil
	}

	// The disk is neither attached nor retained — remove the VM entry from the CVI.
	logger.Info("Removing VM entry from CVI after snapshot deletion",
		"vmName", vmSnapshot.Spec.VMName)

	patch := ctrlclient.MergeFrom(cvi.DeepCopy())
	updated := cvi.Spec.VMs[:0]
	for _, entry := range cvi.Spec.VMs {
		if entry.VMName != vmSnapshot.Spec.VMName {
			updated = append(updated, entry)
		}
	}
	cvi.Spec.VMs = updated

	if err := r.Client.Patch(ctx, cvi, patch); err != nil {
		return fmt.Errorf("failed to patch CVI to remove VM entry for PVC %q: %w", disk.PVCName, err)
	}

	logger.Info("Successfully removed VM entry from CVI", "vmName", vmSnapshot.Spec.VMName)
	return nil
}
