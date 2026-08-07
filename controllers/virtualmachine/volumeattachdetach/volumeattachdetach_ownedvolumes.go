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
	fcdID    string // set iff the disk is still a registered FCD (fcd-retained)
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
		if entry == nil ||
			vmopv1util.NormalizeDiskMode(entry.DiskMode) != plan.DiskMode ||
			entry.VolumeName != plan.VolumeName {
			// Append or update this VM's entry and patch. An already-present
			// entry whose DiskMode or VolumeName differs is updated in
			// place. The DiskMode update is what makes a VKS disk-mode
			// conversion (V12) converge; the VolumeName update backfills
			// entries written before D2 existed (or by migration, V11) so
			// an upgraded operator self-heals before the first detach
			// (attach/detach §4.1.7, V5 risk).
			patch := ctrlclient.MergeFrom(cvi.DeepCopy())
			if entry == nil {
				cvi.Spec.VMs = append(cvi.Spec.VMs, cnsv1alpha1.VirtualMachineRef{
					VMName:         vm.Name,
					VMInstanceUUID: vm.Status.InstanceUUID,
					DiskMode:       plan.DiskMode,
					VolumeName:     plan.VolumeName,
				})
			} else {
				for i := range cvi.Spec.VMs {
					if cvi.Spec.VMs[i].VMName == vm.Name {
						cvi.Spec.VMs[i].DiskMode = plan.DiskMode
						cvi.Spec.VMs[i].VolumeName = plan.VolumeName
					}
				}
			}
			if err := r.Client.Patch(ctx, cvi, patch); err != nil {
				return fmt.Errorf("failed to patch CsiVolumeInfo spec.vms for PVC %s: %w", claimName, err)
			}
			ctx.Logger.Info("Wrote VM entry to CsiVolumeInfo spec.vms for attach",
				"pvc", claimName, "cvi", cvi.Name, "diskMode", plan.DiskMode, "volumeName", plan.VolumeName)
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

		rd := readyDependentDisk{
			plan:     plan,
			diskPath: diskPath,
			diskUUID: cvi.Spec.DiskUUID,
		}
		if vmopv1util.IsFcdRetained(cvi) {
			// The FCD was never unregistered — it is still an FCD identity
			// vpxd can use for its linked-clone precheck (attach/detach
			// §7.1.5). CBT-per-disk does not apply here: §7.1.4 reserves
			// that directive for independent disks, and the platform's
			// default already produces the correct row for a dependent
			// fcd-retained disk.
			rd.fcdID = cvi.Spec.VolumeID
		}
		ready = append(ready, rd)
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
			FcdID:               rd.fcdID,
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

// detachOwnedVolume detaches the disk backed by the given CsiVolumeInfo from
// the VM (if still present), removes this VM's entry from the CVI, and
// clears the corresponding VM status entry. Handles both ownership
// behaviors (attach/detach §8.2 dependent, §8.5 independent).
//
// The device is located by pairing the CVI entry's volumeName to the
// vm.status.volumes entry it names, never by diskUUID (§4.2.2) — a diskUUID
// pairing returns nothing for an fcd-retained volume, whose spec.diskUUID is
// empty, and cannot survive more than one volume being removed from
// vm.spec.volumes in the same edit. If volumeName resolves to no status
// entry — a stale value, a status wiped by a snapshot revert, or a slot
// never observed — no ReconfigVM is issued; the volume falls through to the
// same disk-not-on-VM handling as a disk that was already removed
// out-of-band.
func (r *Reconciler) detachOwnedVolume(
	ctx *pkgctx.VolumeContext,
	cvi *cnsv1alpha1.CsiVolumeInfo) error {

	vm := ctx.VM

	entry := vmopv1util.VMEntry(cvi, vm.Name)
	if entry == nil {
		// Caller already checked this; nothing to do.
		return nil
	}
	dependent := vmopv1util.IsDependentMode(entry.DiskMode)

	statusEntry := findVolumeStatusByName(vm, entry.VolumeName)

	if statusEntry == nil ||
		statusEntry.ControllerBusNumber == nil ||
		statusEntry.UnitNumber == nil {
		return r.removeCVIEntryIfNotRetained(ctx, cvi, statusEntry, dependent)
	}

	if !dependent {
		// Independent: remove the device and the entry, done. No diskPath
		// refresh — the FCD stays registered and CSIManaged, so
		// spec.diskPath is never consumed for it — and no re-registration
		// follows (§8.5). The finalizer and fcd-retained machinery never
		// apply; they exist only for VMManaged (dependent) volumes.
		if _, err := r.VMProvider.DetachDiskAtSlot(
			ctx, vm,
			statusEntry.ControllerType,
			*statusEntry.ControllerBusNumber,
			*statusEntry.UnitNumber,
		); err != nil {
			return fmt.Errorf("failed to detach independent disk for CsiVolumeInfo %s: %w", cvi.Name, err)
		}
		ctx.Logger.Info("Removed independent VM-owned disk from VM",
			"cvi", cvi.Name, "pvc", cvi.Spec.PVCName)

		return r.removeCVIEntryIfNotRetained(ctx, cvi, statusEntry, false)
	}

	// Dependent: refresh spec.diskPath from the live device BEFORE the
	// remove. After the device is gone the live VM no longer carries the
	// path (§8.2 B.2) — this ordering is what makes re-registration correct
	// after a storage vMotion. The value written is the device's own
	// backing path, not its root ancestor; base-walking here would feed the
	// wrong path back to CSI.
	livePath, err := r.VMProvider.GetLiveDiskPathAtSlot(
		ctx, vm,
		statusEntry.ControllerType,
		*statusEntry.ControllerBusNumber,
		*statusEntry.UnitNumber,
	)
	if err != nil {
		return fmt.Errorf("failed to read live disk path for CsiVolumeInfo %s: %w", cvi.Name, err)
	}
	if livePath != "" && livePath != cvi.Spec.DiskPath {
		patch := ctrlclient.MergeFrom(cvi.DeepCopy())
		cvi.Spec.DiskPath = livePath
		if err := r.Client.Patch(ctx, cvi, patch); err != nil {
			return fmt.Errorf("failed to refresh CsiVolumeInfo diskPath before detach for %s: %w", cvi.Name, err)
		}
	}

	if _, err := r.VMProvider.DetachDiskAtSlot(
		ctx, vm,
		statusEntry.ControllerType,
		*statusEntry.ControllerBusNumber,
		*statusEntry.UnitNumber,
	); err != nil {
		return fmt.Errorf("failed to detach disk for CsiVolumeInfo %s: %w", cvi.Name, err)
	}

	ctx.Logger.Info("Removed VM-owned disk from VM", "cvi", cvi.Name, "pvc", cvi.Spec.PVCName)

	return r.removeCVIEntryIfNotRetained(ctx, cvi, statusEntry, dependent)
}

// removeCVIEntryIfNotRetained removes this VM's entry from the CVI — so CSI
// can re-register the volume — and the corresponding VM status entry, if
// any. It does neither while a VM snapshot still retains the disk: the
// entry is a hold that must persist, and removing it would trigger a
// premature (and failing) re-registration (spec §5.4, §11.2 E.5).
//
// The retention check itself is skipped for an independent disk rather than
// evaluated and discarded: independent disks are excluded from VM snapshots
// by vSphere, so the check is a guaranteed false for them, and skipping it
// avoids paying for a snapshot-tree walk on every independent detach.
func (r *Reconciler) removeCVIEntryIfNotRetained(
	ctx *pkgctx.VolumeContext,
	cvi *cnsv1alpha1.CsiVolumeInfo,
	statusEntry *vmopv1.VirtualMachineVolumeStatus,
	dependent bool) error {

	vm := ctx.VM

	var retained bool
	if dependent {
		var err error
		retained, err = vmopv1util.IsDiskRetainedBySnapshot(
			ctx, r.Client, r.VMProvider, ctx.Logger, vm, "", cvi.Spec.PVCName, cvi.Spec.DiskUUID)
		if err != nil {
			return fmt.Errorf("failed to check snapshot retention for CsiVolumeInfo %s: %w", cvi.Name, err)
		}
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
	cvi.Spec.VMs = vmopv1util.RemoveVMEntry(cvi.Spec.VMs, vm.Name)
	if err := r.Client.Patch(ctx, cvi, patch); err != nil {
		return fmt.Errorf("failed to patch CsiVolumeInfo %s during detach: %w", cvi.Name, err)
	}
	if statusEntry != nil {
		r.removeVolumeStatus(ctx, statusEntry.Name)
	}
	return nil
}

// findVolumeStatusByName returns the managed VM status volume entry with the
// given name, or nil if none match (or the name is empty — an entry whose
// volumeName has not yet been backfilled).
func findVolumeStatusByName(vm *vmopv1.VirtualMachine, volumeName string) *vmopv1.VirtualMachineVolumeStatus {
	if volumeName == "" {
		return nil
	}
	for i := range vm.Status.Volumes {
		vs := &vm.Status.Volumes[i]
		if vs.Type == vmopv1.VolumeTypeManaged && vs.Name == volumeName {
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
		cvi.Spec.VMs = vmopv1util.RemoveVMEntry(cvi.Spec.VMs, vm.Name)
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
