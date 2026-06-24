// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package virtualmachinesnapshot

import (
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha6"
	backupapi "github.com/vmware-tanzu/vm-operator/pkg/backup/api"
	pkgctx "github.com/vmware-tanzu/vm-operator/pkg/context"
	vmopv1util "github.com/vmware-tanzu/vm-operator/pkg/util/vmopv1"
)

// reconcileGreenfieldSnapshotDeletion evaluates CsiVolumeInfo entries for
// greenfield volumes retained by the snapshot being deleted. It removes a
// VM's entry from spec.vms for each disk that is no longer attached to the
// VM and not retained by any remaining snapshot.
//
// Called from ReconcileDelete after the vSphere snapshot has been deleted.
//
// NOTE: This covers managed snapshots only (those with a VirtualMachineSnapshot
// CR). Unmanaged snapshots (out-of-band, no CR) require a vCenter snapshot tree
// query as the authoritative backstop; that is left for a follow-up.
func (r *Reconciler) reconcileGreenfieldSnapshotDeletion(
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

	return utilerrors.NewAggregate(errs)
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

	// Check if any remaining snapshot (managed or unmanaged) still retains this disk.
	retained, err := r.isDiskRetainedByRemainingSnapshot(ctx, vm, vmSnapshot.Name, disk.PVCName, disk.UUID)
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

// isDiskRetainedByRemainingSnapshot returns true if any snapshot — managed or
// unmanaged — retains the given disk. It runs in two tiers:
//
//  1. Fast path: scans remaining VirtualMachineSnapshot CRs for the PVC name
//     in their PVCDiskData ExtraConfig. This covers managed snapshots quickly.
//
//  2. Authoritative backstop: queries the live vCenter snapshot tree and
//     checks every node (including unmanaged snapshots with no CR) for a
//     VirtualDisk whose backing UUID matches the disk recorded in pvcDisk.
//     This tier is required — unmanaged snapshots are invisible to the fast
//     path yet can still pin the VMDK.
func (r *Reconciler) isDiskRetainedByRemainingSnapshot(
	ctx *pkgctx.VirtualMachineSnapshotContext,
	vm *vmopv1.VirtualMachine,
	deletingSnapshotName, pvcName, diskUUID string) (bool, error) {

	// --- Fast path: managed VMSnapshot CRs ---
	var snapshotList vmopv1.VirtualMachineSnapshotList
	if err := r.Client.List(ctx, &snapshotList,
		ctrlclient.InNamespace(ctx.VirtualMachineSnapshot.Namespace)); err != nil {
		return false, fmt.Errorf("failed to list VirtualMachineSnapshots: %w", err)
	}

	for i := range snapshotList.Items {
		snap := &snapshotList.Items[i]

		if snap.Name == deletingSnapshotName {
			continue
		}
		if snap.Spec.VMName != vm.Name {
			continue
		}

		snapDisks, err := r.VMProvider.GetPVCDiskDataFromSnapshot(ctx, vm, snap.Name)
		if err != nil {
			ctx.Logger.V(4).Info("Failed to read PVC disk data from snapshot, skipping",
				"snapshotName", snap.Name, "error", err.Error())
			continue
		}

		for _, d := range snapDisks {
			if d.PVCName == pvcName {
				return true, nil
			}
		}
	}

	// --- Authoritative backstop: live vCenter snapshot tree ---
	// This covers unmanaged snapshots (no CR) that are invisible above.
	if diskUUID == "" {
		// No UUID available — cannot perform the vCenter check. Treat as
		// not retained to avoid permanently blocking re-registration, but
		// log so the operator can investigate.
		ctx.Logger.Info("Disk UUID is empty; skipping vCenter snapshot tree check",
			"pvcName", pvcName)
		return false, nil
	}

	retained, err := r.VMProvider.IsDiskRetainedByAnySnapshot(ctx, vm, diskUUID)
	if err != nil {
		return false, fmt.Errorf("failed to check vCenter snapshot tree for disk %s: %w", diskUUID, err)
	}

	return retained, nil
}
