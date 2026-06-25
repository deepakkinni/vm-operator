// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package volumeattachdetach

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

// reconcileOwnedVolumes reconciles CsiVolumeInfo-based volume attach/detach
// for VM-owned-volumes VMs. It is called from ReconcileNormal when the VM has the
// VMOwnedVolumes annotation. It handles Workflow A (attach) and Workflow B
// (detach).
func (r *Reconciler) reconcileOwnedVolumes(ctx *pkgctx.VolumeContext) error {
	vm := ctx.VM

	// Ensure the CVI cleanup finalizer is present so that CsiVolumeInfo entries
	// are cleaned up before the VM CR is garbage-collected.
	if !controllerutil.ContainsFinalizer(vm, pkgconst.CVICleanupFinalizer) {
		controllerutil.AddFinalizer(vm, pkgconst.CVICleanupFinalizer)
		ctx.Logger.Info("Added CVICleanupFinalizer to VM-owned-volumes VM")
		// Return immediately so the patch helper persists the finalizer before
		// any further reconciliation.
		return nil
	}

	// Build a set of PVC claim names currently referenced by spec.volumes for
	// quick lookup. Detach is driven off the CsiVolumeInfo entries that
	// reference this VM whose PVC is no longer in spec, so the PVC claim name
	// (not the volume name) is the correct key.
	specClaimNames := make(map[string]struct{}, len(vm.Spec.Volumes))
	for _, vol := range vm.Spec.Volumes {
		if vol.PersistentVolumeClaim != nil {
			specClaimNames[vol.PersistentVolumeClaim.ClaimName] = struct{}{}
		}
	}

	// Workflow B — Detach volumes referenced by a CsiVolumeInfo entry for this
	// VM whose PVC is no longer in spec.volumes.
	if err := r.reconcileOwnedVolumeDetach(ctx, specClaimNames); err != nil {
		return err
	}

	// Workflow A — Attach volumes that are in spec but NOT in status.
	if err := r.reconcileOwnedVolumeAttach(ctx); err != nil {
		return err
	}

	return nil
}

// reconcileOwnedVolumeAttach processes Workflow A: for each volume in spec.volumes
// that does not yet appear in status.volumes with an attached disk, write the
// VM entry to the CsiVolumeInfo spec.vms and, once CSI signals green, attach
// the disk directly to the VM via ReconfigVM.
//
// All dependent-persistent volumes are processed in a single pass. Volumes
// whose CVI is not yet ready (entry just patched, or green signal absent) set
// needRequeue and continue — they do not block later volumes that are ready.
// A single RequeueError is returned at the end if any volume is still pending.
func (r *Reconciler) reconcileOwnedVolumeAttach(ctx *pkgctx.VolumeContext) error {

	vm := ctx.VM

	// Build a set of volume names that already have a status entry (attached).
	statusVolumeNames := make(map[string]struct{}, len(vm.Status.Volumes))
	for _, vs := range vm.Status.Volumes {
		if vs.DiskUUID != "" {
			statusVolumeNames[vs.Name] = struct{}{}
		}
	}

	needRequeue := false

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
				ctx.Logger.Info("No CsiVolumeInfo for PVC — skipping vm-owned volumes path",
					"pvc", claimName)
				continue
			}
			return fmt.Errorf("failed to get CsiVolumeInfo for PVC %s: %w", claimName, err)
		}

		if !vmopv1util.HasVMEntry(cvi, vm.Name) {
			// Append this VM to spec.vms and patch. CSI will react and update
			// status (ownership transfer / green signal). Continue processing
			// remaining volumes — other CVIs are independent.
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
			needRequeue = true
			continue
		}

		if !vmopv1util.IsGreenSignal(cvi) {
			ctx.Logger.Info("Waiting for CSI to unregister volume (green signal not yet present)",
				"pvc", claimName, "cvi", cvi.Name)
			needRequeue = true
			continue
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

		// Record the diskUUID from the CVI so that the detach path can correlate
		// this VM status entry back to the CsiVolumeInfo after the volume is
		// removed from spec.volumes. Slot info is observed by the VM controller
		// on a later reconcile.
		// Attached is set to true immediately: AttachOrphanedDiskToVM only
		// returns here on a successful ReconfigVM, so the disk is physically
		// present. session.reconcileVolumes gates power-on on Attached=true for
		// every PVC-backed volume, so setting it here unblocks the first
		// power-on after attach.
		vm.Status.Volumes = append(vm.Status.Volumes, vmopv1.VirtualMachineVolumeStatus{
			Name:     vol.Name,
			Type:     vmopv1.VolumeTypeManaged,
			DiskUUID: cvi.Spec.DiskUUID,
			Attached: true,
		})
	}

	if needRequeue {
		return pkgerr.RequeueError{
			After:   5 * time.Second,
			Message: "waiting for CSI to process CsiVolumeInfo updates",
		}
	}

	return nil
}

// reconcileOwnedVolumeDetach processes Workflow B: for each CsiVolumeInfo that
// has a spec.vms entry for this VM but whose bound PVC is no longer referenced
// by vm.spec.volumes, detach the disk from the VM and remove the VM entry from
// the CsiVolumeInfo.
//
// Detach is driven off the CsiVolumeInfo entries (the PVC claim name carried in
// spec.pvc), not the VM status volume name, because the volume name and the PVC
// claim name are distinct and the removed volume is no longer present in
// vm.spec.volumes to correlate the two.
func (r *Reconciler) reconcileOwnedVolumeDetach(
	ctx *pkgctx.VolumeContext,
	specClaimNames map[string]struct{}) error {

	vm := ctx.VM

	// List all CsiVolumeInfo CRs in the system namespace that have an entry for
	// this VM. The CVI lives in the CSI system namespace, so the list is scoped
	// there and then filtered by spec.pvcNamespace and spec.vms.
	cviList := &cnsv1alpha1.CsiVolumeInfoList{}
	if err := r.Client.List(ctx, cviList,
		ctrlclient.InNamespace(pkgconst.CVISystemNamespace)); err != nil {
		return fmt.Errorf("failed to list CsiVolumeInfo objects: %w", err)
	}

	for i := range cviList.Items {
		cvi := &cviList.Items[i]

		// Only consider CVIs for PVCs in this VM's namespace that reference this VM.
		if cvi.Spec.PVCNamespace != vm.Namespace {
			continue
		}
		if !vmopv1util.HasVMEntry(cvi, vm.Name) {
			continue
		}

		// If the PVC is still referenced by vm.spec.volumes, the volume remains
		// attached — nothing to detach.
		if _, stillAttached := specClaimNames[cvi.Spec.PVCName]; stillAttached {
			continue
		}

		if err := r.detachOwnedVolume(ctx, cvi); err != nil {
			return err
		}
	}

	return nil
}

// detachOwnedVolume detaches the disk backed by the given CsiVolumeInfo from the
// VM (if still present), removes this VM's entry from the CVI, and clears the
// corresponding VM status entry.
func (r *Reconciler) detachOwnedVolume(
	ctx *pkgctx.VolumeContext,
	cvi *cnsv1alpha1.CsiVolumeInfo) error {

	vm := ctx.VM

	// Correlate the CVI to the VM status entry via the disk UUID, which is
	// recorded on both the CVI (spec.diskUUID) and the VM status volume
	// (status.volumes[*].diskUUID). This is the only reliable correlation key
	// once the volume has been removed from vm.spec.volumes.
	statusEntry := r.findVolumeStatusByDiskUUID(ctx, cvi.Spec.DiskUUID)

	if statusEntry == nil ||
		statusEntry.ControllerBusNumber == nil ||
		statusEntry.UnitNumber == nil {
		// The disk is not (or no longer) present on the VM, or slot info is not
		// yet observed. Remove the VM entry from the CVI so CSI can re-register
		// once all relationships are gone.
		patch := ctrlclient.MergeFrom(cvi.DeepCopy())
		cvi.Spec.VMs = removeVMEntry(cvi.Spec.VMs, vm.Name)
		if err := r.Client.Patch(ctx, cvi, patch); err != nil {
			return fmt.Errorf("failed to patch CsiVolumeInfo %s during detach: %w", cvi.Name, err)
		}
		if statusEntry != nil {
			r.removeVolumeStatus(ctx, statusEntry.Name)
		}
		return nil
	}

	diskPath, err := r.VMProvider.DetachDiskAtSlot(
		ctx, vm,
		statusEntry.ControllerType,
		*statusEntry.ControllerBusNumber,
		*statusEntry.UnitNumber,
	)
	if err != nil {
		return fmt.Errorf("failed to detach disk for CsiVolumeInfo %s: %w", cvi.Name, err)
	}

	ctx.Logger.Info("Removed VM-owned disk from VM",
		"cvi", cvi.Name, "pvc", cvi.Spec.PVCName, "diskPath", diskPath)

	// Update CsiVolumeInfo: refresh diskPath if it changed, remove VM entry.
	patch := ctrlclient.MergeFrom(cvi.DeepCopy())
	if diskPath != "" && diskPath != cvi.Spec.DiskPath {
		cvi.Spec.DiskPath = diskPath
	}
	cvi.Spec.VMs = removeVMEntry(cvi.Spec.VMs, vm.Name)
	if err := r.Client.Patch(ctx, cvi, patch); err != nil {
		return fmt.Errorf("failed to patch CsiVolumeInfo after detach for %s: %w", cvi.Name, err)
	}

	r.removeVolumeStatus(ctx, statusEntry.Name)

	return nil
}

// findVolumeStatusByDiskUUID returns the managed VM status volume entry whose
// DiskUUID matches the given UUID, or nil if none match (or the UUID is empty).
func (r *Reconciler) findVolumeStatusByDiskUUID(
	ctx *pkgctx.VolumeContext,
	diskUUID string) *vmopv1.VirtualMachineVolumeStatus {

	if diskUUID == "" {
		return nil
	}
	for i := range ctx.VM.Status.Volumes {
		vs := &ctx.VM.Status.Volumes[i]
		if vs.Type == vmopv1.VolumeTypeManaged && vs.DiskUUID == diskUUID {
			return vs
		}
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

// reconcileOwnedVolumeDelete cleans up CsiVolumeInfo entries for a VM-owned-volumes VM
// that is being deleted. It removes this VM's entry from each CVI that still
// references it, then removes the CVICleanupFinalizer so the VM CR can be
// garbage-collected.
func (r *Reconciler) reconcileOwnedVolumeDelete(ctx *pkgctx.VolumeContext) error {
	vm := ctx.VM

	if !controllerutil.ContainsFinalizer(vm, pkgconst.CVICleanupFinalizer) {
		// Nothing to clean up — finalizer already removed.
		return nil
	}

	ctx.Logger.Info("Reconciling VM-owned-volumes VM deletion: cleaning up CsiVolumeInfo entries")

	// Iterate ALL CsiVolumeInfo CRs that reference this VM (per spec §13.5.2),
	// not just those for volumes still in vm.spec.volumes. A CVI entry may
	// linger for a volume already removed from spec (e.g. snapshot-retained or
	// an in-flight detach), and those must also be cleaned up before the VM CR
	// is garbage-collected.
	cviList := &cnsv1alpha1.CsiVolumeInfoList{}
	if err := r.Client.List(ctx, cviList,
		ctrlclient.InNamespace(pkgconst.CVISystemNamespace)); err != nil {
		return fmt.Errorf("failed to list CsiVolumeInfo objects during VM deletion: %w", err)
	}

	for i := range cviList.Items {
		cvi := &cviList.Items[i]

		if cvi.Spec.PVCNamespace != vm.Namespace {
			continue
		}
		if !vmopv1util.HasVMEntry(cvi, vm.Name) {
			continue
		}

		patch := ctrlclient.MergeFrom(cvi.DeepCopy())
		cvi.Spec.VMs = removeVMEntry(cvi.Spec.VMs, vm.Name)
		if err := r.Client.Patch(ctx, cvi, patch); err != nil {
			return fmt.Errorf("failed to patch CsiVolumeInfo %s during VM deletion: %w",
				cvi.Name, err)
		}

		ctx.Logger.Info("Removed VM entry from CsiVolumeInfo during VM deletion",
			"pvc", cvi.Spec.PVCName, "cvi", cvi.Name)
	}

	// All CVI entries cleaned up — remove the finalizer so the VM CR can be deleted.
	controllerutil.RemoveFinalizer(vm, pkgconst.CVICleanupFinalizer)
	ctx.Logger.Info("Removed CVICleanupFinalizer from VM-owned-volumes VM")

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
