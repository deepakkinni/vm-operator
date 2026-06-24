// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package volume

import (
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha6"
	cnsv1alpha1 "github.com/vmware-tanzu/vm-operator/external/vsphere-csi-driver/api/v1alpha1"
	pkgconst "github.com/vmware-tanzu/vm-operator/pkg/constants"
	pkgctx "github.com/vmware-tanzu/vm-operator/pkg/context"
	pkgerr "github.com/vmware-tanzu/vm-operator/pkg/errors"
	vmopv1util "github.com/vmware-tanzu/vm-operator/pkg/util/vmopv1"
)

// reconcileGreenfieldVolumes reconciles CsiVolumeInfo-based volume attach/detach
// for greenfield VMs. It is called from ReconcileNormal when the VM has the
// VMOwnedVolumes annotation. It handles Workflow A (attach) and Workflow B
// (detach).
func (r *Reconciler) reconcileGreenfieldVolumes(ctx *pkgctx.VolumeContext) error {
	vm := ctx.VM

	// Ensure the CVI cleanup finalizer is present so that CsiVolumeInfo entries
	// are cleaned up before the VM CR is garbage-collected.
	if !controllerutil.ContainsFinalizer(vm, pkgconst.CVICleanupFinalizer) {
		controllerutil.AddFinalizer(vm, pkgconst.CVICleanupFinalizer)
		ctx.Logger.Info("Added CVICleanupFinalizer to greenfield VM")
		// Return immediately so the patch helper persists the finalizer before
		// any further reconciliation.
		return nil
	}

	// Build a set of volume names currently in spec for quick lookup.
	specVolumeNames := make(map[string]struct{}, len(vm.Spec.Volumes))
	for _, vol := range vm.Spec.Volumes {
		specVolumeNames[vol.Name] = struct{}{}
	}

	// Workflow B — Detach volumes that are in status but NOT in spec.
	if err := r.reconcileGreenfieldDetach(ctx, specVolumeNames); err != nil {
		return err
	}

	// Workflow A — Attach volumes that are in spec but NOT in status.
	if err := r.reconcileGreenfieldAttach(ctx, specVolumeNames); err != nil {
		return err
	}

	return nil
}

// reconcileGreenfieldAttach processes Workflow A: for each volume in spec.volumes
// that does not yet appear in status.volumes with an attached disk, write the
// VM entry to the CsiVolumeInfo spec.vms and, once CSI signals green, attach
// the disk directly to the VM via ReconfigVM.
func (r *Reconciler) reconcileGreenfieldAttach(
	ctx *pkgctx.VolumeContext,
	specVolumeNames map[string]struct{}) error {

	vm := ctx.VM

	// Build a set of volume names that already have a status entry (attached).
	statusVolumeNames := make(map[string]struct{}, len(vm.Status.Volumes))
	for _, vs := range vm.Status.Volumes {
		if vs.DiskUUID != "" {
			statusVolumeNames[vs.Name] = struct{}{}
		}
	}

	for _, vol := range vm.Spec.Volumes {
		if vol.PersistentVolumeClaim == nil {
			continue
		}

		// Only dependent-persistent mode uses the VM-owned path.
		if !vmopv1util.IsDependentPersistentMode(vol) {
			continue
		}

		// Already attached — nothing to do.
		if _, ok := statusVolumeNames[vol.Name]; ok {
			continue
		}

		claimName := vol.PersistentVolumeClaim.ClaimName

		cvi, err := vmopv1util.GetCVIForPVC(ctx, r.Client, vm.Namespace, claimName)
		if err != nil {
			if apierrors.IsNotFound(err) {
				// Brownfield PVC without a CsiVolumeInfo — skip; legacy path handles it.
				ctx.Logger.Info("No CsiVolumeInfo for PVC — skipping greenfield path",
					"pvc", claimName)
				continue
			}
			return fmt.Errorf("failed to get CsiVolumeInfo for PVC %s: %w", claimName, err)
		}

		if !vmopv1util.HasVMEntry(cvi, vm.Name) {
			// Append this VM to spec.vms and patch.
			patch := ctrlclient.MergeFrom(cvi.DeepCopy())
			cvi.Spec.VMs = append(cvi.Spec.VMs, cnsv1alpha1.CsiVolumeInfoVMEntry{
				VMName:         vm.Name,
				VMInstanceUUID: vm.Status.InstanceUUID,
			})
			if err := r.Client.Patch(ctx, cvi, patch); err != nil {
				return fmt.Errorf("failed to patch CsiVolumeInfo spec.vms for PVC %s: %w", claimName, err)
			}
			ctx.Logger.Info("Appended VM entry to CsiVolumeInfo spec.vms for attach",
				"pvc", claimName, "cvi", cvi.Name)
			// Requeue — CSI will react and update status.
			return pkgerr.RequeueError{
				After:   5 * time.Second,
				Message: "waiting for CSI to process CsiVolumeInfo spec.vms update",
			}
		}

		if !vmopv1util.IsGreenSignal(cvi) {
			ctx.Logger.Info("Waiting for CSI to unregister volume (green signal not yet present)",
				"pvc", claimName, "cvi", cvi.Name)
			return pkgerr.RequeueError{
				After:   5 * time.Second,
				Message: "waiting for CSI green signal on CsiVolumeInfo",
			}
		}

		// Green signal present — attach the disk to the VM.
		diskPath := cvi.Spec.DiskPath
		if diskPath == "" {
			return fmt.Errorf("CsiVolumeInfo %s has empty diskPath after green signal", cvi.Name)
		}

		if err := r.VMProvider.AttachOrphanedDiskToVM(ctx, vm, diskPath); err != nil {
			return fmt.Errorf("failed to attach orphaned disk for PVC %s: %w", claimName, err)
		}

		ctx.Logger.Info("Added VM-owned disk to VM", "pvc", claimName, "diskPath", diskPath)

		vm.Status.Volumes = append(vm.Status.Volumes, vmopv1.VirtualMachineVolumeStatus{
			Name: vol.Name,
			Type: vmopv1.VolumeTypeManaged,
		})
	}

	return nil
}

// reconcileGreenfieldDetach processes Workflow B: for each volume in
// status.volumes that is NOT in spec.volumes, detach the disk from the VM and
// remove the VM entry from the CsiVolumeInfo.
func (r *Reconciler) reconcileGreenfieldDetach(
	ctx *pkgctx.VolumeContext,
	specVolumeNames map[string]struct{}) error {

	vm := ctx.VM

	// Collect volumes in status that are no longer in spec.
	var toDetach []vmopv1.VirtualMachineVolumeStatus
	for _, vs := range vm.Status.Volumes {
		if vs.Type != vmopv1.VolumeTypeManaged {
			continue
		}
		if _, ok := specVolumeNames[vs.Name]; !ok {
			toDetach = append(toDetach, vs)
		}
	}

	for _, vs := range toDetach {
		// The volume name matches a spec volume name that no longer exists. To
		// resolve the CVI we need the PVC claim name. The volume name IS the
		// spec volume name, but we need the PVC claim name. Because the volume
		// has been removed from spec we need to look it up via the status entry.
		// The status entry doesn't store the PVC claim name directly, so we
		// attempt lookup using the volume name as both volume-name and claim-name
		// (a reasonable convention); if not found we skip.
		cvi, err := vmopv1util.GetCVIForPVC(ctx, r.Client, vm.Namespace, vs.Name)
		if err != nil {
			if apierrors.IsNotFound(err) {
				ctx.Logger.Info("CsiVolumeInfo not found for detach — skipping",
					"volumeName", vs.Name)
				r.removeVolumeStatus(ctx, vs.Name)
				continue
			}
			return fmt.Errorf("failed to get CsiVolumeInfo for volume %s: %w", vs.Name, err)
		}

		if !vmopv1util.HasVMEntry(cvi, vm.Name) {
			// Already cleaned up — remove from status.
			r.removeVolumeStatus(ctx, vs.Name)
			continue
		}

		// Detach the disk from the VM.
		if vs.ControllerBusNumber == nil || vs.UnitNumber == nil {
			ctx.Logger.Info("Volume status missing slot info — skipping detach",
				"volumeName", vs.Name)
			continue
		}

		diskPath, err := r.VMProvider.DetachDiskAtSlot(
			ctx, vm,
			vs.ControllerType,
			*vs.ControllerBusNumber,
			*vs.UnitNumber,
		)
		if err != nil {
			return fmt.Errorf("failed to detach disk for volume %s: %w", vs.Name, err)
		}

		ctx.Logger.Info("Removed VM-owned disk from VM", "volumeName", vs.Name, "diskPath", diskPath)

		// Update CsiVolumeInfo: refresh diskPath if it changed, remove VM entry.
		patch := ctrlclient.MergeFrom(cvi.DeepCopy())
		if diskPath != "" && diskPath != cvi.Spec.DiskPath {
			cvi.Spec.DiskPath = diskPath
		}
		cvi.Spec.VMs = removeVMEntry(cvi.Spec.VMs, vm.Name)
		if err := r.Client.Patch(ctx, cvi, patch); err != nil {
			return fmt.Errorf("failed to patch CsiVolumeInfo after detach for volume %s: %w", vs.Name, err)
		}

		r.removeVolumeStatus(ctx, vs.Name)
	}

	return nil
}

// removeVolumeStatus removes the status entry with the given name from
// ctx.VM.Status.Volumes in place.
func (r *Reconciler) removeVolumeStatus(ctx *pkgctx.VolumeContext, name string) {
	volumes := ctx.VM.Status.Volumes[:0]
	for _, vs := range ctx.VM.Status.Volumes {
		if vs.Name != name {
			volumes = append(volumes, vs)
		}
	}
	ctx.VM.Status.Volumes = volumes
}

// reconcileGreenfieldDelete cleans up CsiVolumeInfo entries for a greenfield VM
// that is being deleted. It removes this VM's entry from each CVI that still
// references it, then removes the CVICleanupFinalizer so the VM CR can be
// garbage-collected.
func (r *Reconciler) reconcileGreenfieldDelete(ctx *pkgctx.VolumeContext) error {
	vm := ctx.VM

	if !controllerutil.ContainsFinalizer(vm, pkgconst.CVICleanupFinalizer) {
		// Nothing to clean up — finalizer already removed.
		return nil
	}

	ctx.Logger.Info("Reconciling greenfield VM deletion: cleaning up CsiVolumeInfo entries")

	for _, vol := range vm.Spec.Volumes {
		if vol.PersistentVolumeClaim == nil {
			continue
		}

		claimName := vol.PersistentVolumeClaim.ClaimName
		cvi, err := vmopv1util.GetCVIForPVC(ctx, r.Client, vm.Namespace, claimName)
		if err != nil {
			if apierrors.IsNotFound(err) {
				ctx.Logger.Info("CsiVolumeInfo not found for PVC during VM deletion — skipping",
					"pvc", claimName)
				continue
			}
			return fmt.Errorf("failed to get CsiVolumeInfo for PVC %s during VM deletion: %w",
				claimName, err)
		}

		if !vmopv1util.HasVMEntry(cvi, vm.Name) {
			// Already removed.
			continue
		}

		patch := ctrlclient.MergeFrom(cvi.DeepCopy())
		cvi.Spec.VMs = removeVMEntry(cvi.Spec.VMs, vm.Name)
		if err := r.Client.Patch(ctx, cvi, patch); err != nil {
			return fmt.Errorf("failed to patch CsiVolumeInfo for PVC %s during VM deletion: %w",
				claimName, err)
		}

		ctx.Logger.Info("Removed VM entry from CsiVolumeInfo during VM deletion",
			"pvc", claimName, "cvi", cvi.Name)
	}

	// All CVI entries cleaned up — remove the finalizer so the VM CR can be deleted.
	controllerutil.RemoveFinalizer(vm, pkgconst.CVICleanupFinalizer)
	ctx.Logger.Info("Removed CVICleanupFinalizer from greenfield VM")

	return nil
}

// removeVMEntry returns a new slice with the entry for vmName removed.
func removeVMEntry(entries []cnsv1alpha1.CsiVolumeInfoVMEntry, vmName string) []cnsv1alpha1.CsiVolumeInfoVMEntry {
	result := entries[:0]
	for _, e := range entries {
		if e.VMName != vmName {
			result = append(result, e)
		}
	}
	return result
}
