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
	"github.com/vmware-tanzu/vm-operator/pkg/providers"
	vmopv1util "github.com/vmware-tanzu/vm-operator/pkg/util/vmopv1"
	"github.com/vmware-tanzu/vm-operator/pkg/volumes/owned"
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

// readyDependentDisk pairs a ready-to-attach dependent volume with the
// resolved backing path and mode needed to build its device.
type readyDependentDisk struct {
	plan     owned.VolumePlan
	diskPath string
	diskUUID string // spec.diskUUID, informational (§4.2.2); "" when fcd-retained
}

// reconcileOwnedVolumeAttach processes Workflow A: for each volume in spec.volumes
// that does not yet appear in status.volumes with an attached disk, write the
// VM entry to the CsiVolumeInfo spec.vms and, once ready, attach the disk
// directly to the VM via ReconfigVM.
//
// Every PVC-backed volume on a VM-owned VM is processed in a single pass,
// regardless of disk mode (attach/detach §2.7) — disk mode only selects the
// ownership behavior. Volumes whose CVI is not yet ready (entry just
// patched, or a dependent volume's green signal absent) set needRequeue and
// continue — they do not block later volumes that are ready. Ready dependent
// volumes are collected and attached in a single ReconfigVM_Task (attach/detach
// §7.3 note); a single RequeueError is returned at the end if any volume is
// still pending.
//
// Independent-mode device attach (backing resolution, vDiskId, per-disk CBT)
// is not yet implemented — it depends on a CNS/vslm client this codebase does
// not have. The entry is written to the CVI; the device add itself requeues
// indefinitely until that dependency lands (see V3/V4 in the implementation
// plan).
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
	ready := make([]readyDependentDisk, 0, len(vm.Spec.Volumes))

	for _, plan := range owned.ClassifyVolumes(vm) {
		// Already attached — nothing to do.
		if _, ok := statusVolumeNames[plan.VolumeName]; ok {
			continue
		}

		claimName := plan.ClaimName

		// A missing CsiVolumeInfo on a VM-owned VM is an anomaly to repair,
		// not a brownfield PVC to skip (attach/detach §4.1.2, §13.1) — so
		// this creates it rather than looking it up read-only. Only an
		// unresolvable PVC or PV (not yet bound, or genuinely orphaned) is
		// skipped.
		cvi, err := vmopv1util.EnsureCVIForPVC(ctx, r.Client, vm.Namespace, claimName)
		if err != nil {
			if apierrors.IsNotFound(err) {
				ctx.Logger.Info("PVC or PV not resolvable — skipping vm-owned volumes path for now",
					"pvc", claimName)
				continue
			}
			return fmt.Errorf("failed to ensure CsiVolumeInfo for PVC %s: %w", claimName, err)
		}

		entry := vmopv1util.VMEntry(cvi, vm.Name)
		if entry == nil || vmopv1util.NormalizeDiskMode(entry.DiskMode) != plan.DiskMode {
			// Append or update this VM's entry and patch. An already-present
			// entry whose DiskMode differs is updated in place — this is what
			// makes a VKS disk-mode conversion (V12) converge.
			patch := ctrlclient.MergeFrom(cvi.DeepCopy())
			if entry == nil {
				cvi.Spec.VMs = append(cvi.Spec.VMs, cnsv1alpha1.VirtualMachineRef{
					VMName:         vm.Name,
					VMInstanceUUID: vm.Status.InstanceUUID,
					DiskMode:       plan.DiskMode,
				})
			} else {
				for i := range cvi.Spec.VMs {
					if cvi.Spec.VMs[i].VMName == vm.Name {
						cvi.Spec.VMs[i].DiskMode = plan.DiskMode
					}
				}
			}
			if err := r.Client.Patch(ctx, cvi, patch); err != nil {
				return fmt.Errorf("failed to patch CsiVolumeInfo spec.vms for PVC %s: %w", claimName, err)
			}
			ctx.Logger.Info("Wrote VM entry to CsiVolumeInfo spec.vms for attach",
				"pvc", claimName, "cvi", cvi.Name, "diskMode", plan.DiskMode)
			needRequeue = true
			continue
		}

		if !plan.Dependent {
			// Independent-mode readiness and device attach land in a later
			// change — see the function doc comment. The entry is on the
			// CVI; the device add itself is not yet implemented.
			ctx.Logger.Info("Independent-mode VM-owned volume entry present; device attach pending",
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

		diskPath := cvi.Spec.DiskPath
		if diskPath == "" {
			return fmt.Errorf("CsiVolumeInfo %s has empty diskPath after green signal", cvi.Name)
		}

		ready = append(ready, readyDependentDisk{
			plan:     plan,
			diskPath: diskPath,
			diskUUID: cvi.Spec.DiskUUID,
		})
	}

	if len(ready) > 0 {
		if err := r.attachReadyDisks(ctx, vm, ready); err != nil {
			return err
		}
	}

	if needRequeue {
		return pkgerr.RequeueError{
			After:   5 * time.Second,
			Message: "waiting for CSI to process CsiVolumeInfo updates",
		}
	}

	return nil
}

// attachReadyDisks issues one ReconfigVM_Task for every ready dependent disk
// (attach/detach §7.3 note — as few reconfigures as possible) and writes
// vm.status.volumes from the returned placements, including the observed
// device slot (§7.3 A.6) — populating it here, at attach, is what lets the
// detach path (V5) stop correlating by diskUUID.
func (r *Reconciler) attachReadyDisks(
	ctx *pkgctx.VolumeContext,
	vm *vmopv1.VirtualMachine,
	ready []readyDependentDisk) error {

	disks := make([]providers.VolumeDiskAddSpec, 0, len(ready))
	for _, rd := range ready {
		disks = append(disks, providers.VolumeDiskAddSpec{
			VolumeName:          rd.plan.VolumeName,
			DiskPath:            rd.diskPath,
			DiskMode:            rd.plan.RawDiskMode,
			SharingMode:         rd.plan.SharingMode,
			ControllerType:      rd.plan.ControllerType,
			ControllerBusNumber: rd.plan.ControllerBusNumber,
			UnitNumber:          rd.plan.UnitNumber,
		})
	}

	placements, err := r.VMProvider.AttachVolumeDisks(ctx, vm, disks)
	if err != nil {
		// Surface the failure against every disk in the batch, naming none
		// as the culprit — vCenter rejects the whole ReconfigVM_Task for a
		// malformed spec on any one disk, so triage must start from the
		// error message, not from a partial result.
		return fmt.Errorf("failed to attach %d VM-owned disk(s): %w", len(disks), err)
	}

	placementByVolume := make(map[string]providers.VolumeDiskPlacement, len(placements))
	for _, p := range placements {
		placementByVolume[p.VolumeName] = p
	}

	for _, rd := range ready {
		p, ok := placementByVolume[rd.plan.VolumeName]
		if !ok {
			return fmt.Errorf("no placement returned for volume %q after attach", rd.plan.VolumeName)
		}

		diskUUID := p.DiskUUID
		if diskUUID == "" {
			// fcd-retained: spec.diskUUID is never populated (csi.md C4), so
			// the observed UUID from the device is authoritative.
			diskUUID = rd.diskUUID
		}

		ctx.Logger.Info("Added VM-owned disk to VM",
			"volumeName", rd.plan.VolumeName, "diskPath", rd.diskPath, "diskUUID", diskUUID)

		// Attached is set to true immediately: AttachVolumeDisks only
		// returns here on a successful ReconfigVM, so every disk in the
		// batch is physically present. session.reconcileVolumes gates
		// power-on on Attached=true for every PVC-backed volume, so setting
		// it here unblocks the first power-on after attach.
		vm.Status.Volumes = append(vm.Status.Volumes, vmopv1.VirtualMachineVolumeStatus{
			Name:                rd.plan.VolumeName,
			Type:                vmopv1.VolumeTypeManaged,
			DiskUUID:            diskUUID,
			Attached:            true,
			ControllerType:      p.ControllerType,
			ControllerBusNumber: &p.ControllerBusNumber,
			UnitNumber:          &p.UnitNumber,
		})
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

	// Find every CsiVolumeInfo that references this VM via the indexed
	// lookup — bounded by the VM's own CVI population, not a scan of every
	// CVI in vmware-system-csi (V6; implementation-rules §7).
	cvis, err := vmopv1util.ListCVIsForVM(ctx, r.Client, vm)
	if err != nil {
		return fmt.Errorf("failed to list CsiVolumeInfo objects for VM: %w", err)
	}

	for i := range cvis {
		cvi := &cvis[i]

		if vmopv1util.VMEntry(cvi, vm.Name) == nil {
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
		// yet observed (e.g. status was wiped by a snapshot revert). Remove the
		// VM entry from the CVI so CSI can re-register — but ONLY if no VM
		// snapshot still retains the disk. While a snapshot pins the disk, the
		// entry is a hold that must persist; removing it would trigger a
		// premature (and failing) re-registration (spec §5.4, §11.2 E.5).
		retained, err := vmopv1util.IsDiskRetainedBySnapshot(
			ctx, r.Client, r.VMProvider, ctx.Logger, vm, "", cvi.Spec.PVCName, cvi.Spec.DiskUUID)
		if err != nil {
			return fmt.Errorf("failed to check snapshot retention for CsiVolumeInfo %s: %w", cvi.Name, err)
		}
		if retained {
			ctx.Logger.Info("Disk retained by a VM snapshot; keeping CVI entry",
				"cvi", cvi.Name, "pvc", cvi.Spec.PVCName)
			if statusEntry != nil {
				r.removeVolumeStatus(ctx, statusEntry.Name)
			}
			return nil
		}

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

	// Remove the VM entry only if no VM snapshot still retains the disk. The
	// disk has been removed from the VM, but a snapshot may still pin it — in
	// that case the entry must stay so CSI does not re-register prematurely
	// (spec §5.4, §11.2 E.5).
	retained, err := vmopv1util.IsDiskRetainedBySnapshot(
		ctx, r.Client, r.VMProvider, ctx.Logger, vm, "", cvi.Spec.PVCName, cvi.Spec.DiskUUID)
	if err != nil {
		return fmt.Errorf("failed to check snapshot retention for CsiVolumeInfo %s: %w", cvi.Name, err)
	}

	// Update CsiVolumeInfo: refresh diskPath if it changed, and remove the VM
	// entry unless a snapshot retains the disk.
	patch := ctrlclient.MergeFrom(cvi.DeepCopy())
	if diskPath != "" && diskPath != cvi.Spec.DiskPath {
		cvi.Spec.DiskPath = diskPath
	}
	if retained {
		ctx.Logger.Info("Disk retained by a VM snapshot; keeping CVI entry after detach",
			"cvi", cvi.Name, "pvc", cvi.Spec.PVCName)
	} else {
		cvi.Spec.VMs = removeVMEntry(cvi.Spec.VMs, vm.Name)
	}
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

	// Find every CsiVolumeInfo that references this VM (per spec §13.5.2),
	// not just those for volumes still in vm.spec.volumes. A CVI entry may
	// linger for a volume already removed from spec (e.g. snapshot-retained or
	// an in-flight detach), and those must also be cleaned up before the VM CR
	// is garbage-collected. The indexed lookup is bounded by the VM's own CVI
	// population, not a scan of every CVI in vmware-system-csi (V6;
	// implementation-rules §7).
	cvis, err := vmopv1util.ListCVIsForVM(ctx, r.Client, vm)
	if err != nil {
		return fmt.Errorf("failed to list CsiVolumeInfo objects during VM deletion: %w", err)
	}

	for i := range cvis {
		cvi := &cvis[i]

		if vmopv1util.VMEntry(cvi, vm.Name) == nil {
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
func removeVMEntry(entries []cnsv1alpha1.VirtualMachineRef, vmName string) []cnsv1alpha1.VirtualMachineRef {
	result := entries[:0]
	for _, e := range entries {
		if e.VMName != vmName {
			result = append(result, e)
		}
	}
	return result
}
