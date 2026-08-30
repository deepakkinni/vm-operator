// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package volumeattachdetach

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha6"
	cnsv1alpha1 "github.com/vmware-tanzu/vm-operator/external/vsphere-csi-driver/api/v1alpha1"
	pkgconst "github.com/vmware-tanzu/vm-operator/pkg/constants"
	pkgctx "github.com/vmware-tanzu/vm-operator/pkg/context"
	pkgerr "github.com/vmware-tanzu/vm-operator/pkg/errors"
	"github.com/vmware-tanzu/vm-operator/pkg/providers"
	pkgutil "github.com/vmware-tanzu/vm-operator/pkg/util"
	kubeutil "github.com/vmware-tanzu/vm-operator/pkg/util/kube"
	vmopv1util "github.com/vmware-tanzu/vm-operator/pkg/util/vmopv1"
)

// isMigrationCandidate reports whether ctx.VM should be routed through
// reconcileMigration this reconcile, per migration spec §4.1 (Trigger
// Model). A VM is a candidate when the feature gate is on, it lacks the
// vm-owned-volumes annotation, and either the explicit trigger annotation is
// present or a genuine attach/detach edge is pending against the VM's
// currently-tracked volumes: a non-empty CnsNodeVMBatchAttachment add/remove
// diff (toAdd/toRemove), or a legacy CnsNodeVmAttachment slated for
// deletion (legacyToDelete). This is deliberately edge-triggered, not
// level-triggered on "has a PVC-backed volume" — a stable brownfield VM
// whose tracked volumes haven't changed produces empty diffs on every
// reconcile and must not re-trigger migration.
func isMigrationCandidate(
	vm *vmopv1.VirtualMachine,
	toAdd, toRemove sets.Set[string],
	legacyToDelete []cnsv1alpha1.CnsNodeVmAttachment,
) bool {
	if vmopv1util.HasVMOwnedVolumesAnnotation(vm) {
		return false
	}
	if metav1.HasAnnotation(vm.ObjectMeta, pkgconst.MigrateToVMOwnedAnnotation) {
		return true
	}
	return toAdd.Len() > 0 || toRemove.Len() > 0 || len(legacyToDelete) > 0
}

// reconcileMigration drives a brownfield VM's already-attached disks — both
// BA-tracked and legacy-CnsNodeVmAttachment-tracked — onto the CsiVolumeInfo
// path (migration §4, §7, §12), then flips vm-owned-volumes once every disk
// has landed there (§4.4). It returns pkgerr.RequeueError until migration
// completes, so the caller — ReconcileNormal — never processes the
// triggering attach/detach as a VM-owned workflow against a half-migrated
// VM (§4.1's ordering rule).
//
// ba, legacyAttachments, volumeSpecsForLegacy, and legacyToDelete are the
// same read-only fetches ReconcileNormal computes for its own (non-migration)
// batch/legacy processing, passed in rather than re-fetched here so the
// trigger decision and the migrated-disk set are computed from one
// consistent read (migration §4.1).
func (r *Reconciler) reconcileMigration(
	ctx *pkgctx.VolumeContext,
	ba *cnsv1alpha1.CnsNodeVMBatchAttachment,
	legacyAttachments map[string]cnsv1alpha1.CnsNodeVmAttachment,
	volumeSpecsForLegacy []vmopv1.VirtualMachineVolume,
	legacyToDelete []cnsv1alpha1.CnsNodeVmAttachment,
) error {
	vm := ctx.VM

	legacyDeleteNames := sets.New[string]()
	for _, a := range legacyToDelete {
		legacyDeleteNames.Insert(a.Name)
	}

	// Only legacy-tracked disks that are still desired are migrated. One
	// still slated for deletion is the triggering detach itself (or a
	// stale/deprecated attachment) — it is left for the normal cleanup path
	// once this reconcile falls through, per §4.1's ordering rule and the
	// decision that the triggering volume is not folded into this batch.
	var legacyToMigrate []vmopv1.VirtualMachineVolume
	for _, vol := range volumeSpecsForLegacy {
		attachmentName := pkgutil.CNSAttachmentNameForVolume(vm.Name, vol.Name)
		if !legacyDeleteNames.Has(attachmentName) {
			legacyToMigrate = append(legacyToMigrate, vol)
		}
	}

	// VKS disk-mode conversion (V12, migration §4.5) runs off vm.status.volumes
	// — mechanism-agnostic, so it covers a disk regardless of whether it is
	// BA-tracked, legacy-CnsNodeVmAttachment-tracked, or already attached by
	// some other means — before the ba==nil short-circuit below, so a
	// not-yet-migrated VKS VM's already-attached disks are still evaluated
	// even when this reconcile has nothing BA/legacy-tracked left to migrate.
	if kubeutil.HasCAPILabels(vm.Labels) {
		if err := r.convertVKSDiskModes(ctx, vm); err != nil {
			return err
		}
	}

	if ba == nil && len(legacyToMigrate) == 0 {
		// Nothing currently tracked to migrate — the edge that made this VM
		// a migration candidate was the triggering attach/detach itself
		// (e.g. the VM's very first PVC, or a detach of its only tracked
		// disk). Go straight to Stage 2 so that operation is processed as
		// VM-owned Workflow A/B on the next reconcile.
		ctx.Logger.Info("Migration candidate has nothing pre-existing to migrate")
		return r.completeMigration(ctx, nil)
	}

	if ba != nil && ba.Annotations[pkgconst.VMOwnedMigrationAnnotation] != pkgconst.VMOwnedMigrationInProgress {
		// Stage 1 — freeze (§12.1). Nothing else may proceed until this
		// patch is confirmed observed: if a disk left BA.spec before the
		// freeze landed, CSI would detach a live disk. A legacy-only VM
		// (ba == nil) needs no equivalent freeze: falling through this
		// early-return function is itself what keeps ReconcileNormal's
		// legacy cleanup from running concurrently with migration.
		patch := ctrlclient.MergeFrom(ba.DeepCopy())
		metav1.SetMetaDataAnnotation(&ba.ObjectMeta, pkgconst.VMOwnedMigrationAnnotation, pkgconst.VMOwnedMigrationInProgress)
		if err := r.Client.Patch(ctx, ba, patch); err != nil {
			return fmt.Errorf("failed to freeze CnsNodeVMBatchAttachment %s for migration: %w", ba.Name, err)
		}
		ctx.Logger.Info("Froze CnsNodeVMBatchAttachment for migration", "batchAttachment", ba.Name)
	}

	var (
		toRemove = sets.New[string]()
		allDone  = true
	)

	if ba != nil {
		for i := range ba.Spec.Volumes {
			volSpec := ba.Spec.Volumes[i]
			claimName := volSpec.PersistentVolumeClaim.ClaimName
			diskMode := cnsDiskModeToCVIDiskMode(volSpec.PersistentVolumeClaim.DiskMode)
			if specVol := findVolumeSpecByName(vm, volSpec.Name); specVol != nil {
				// The BA's own DiskMode may be stale relative to a VKS
				// conversion this reconcile just durably patched onto
				// vm.spec.volumes (convertVKSDiskModes, above) — vm.spec is
				// the authoritative source once it has an entry for this
				// volume.
				diskMode = vmopv1util.DiskModeForVolume(*specVol)
			}

			cvi, err := vmopv1util.EnsureCVIForPVC(ctx, r.Client, vm.Namespace, claimName)
			if err != nil {
				if apierrors.IsNotFound(err) {
					ctx.Logger.Info("PVC or PV not resolvable — deferring migration for this disk",
						"pvc", claimName)
					allDone = false
					continue
				}
				return fmt.Errorf("failed to ensure CsiVolumeInfo for PVC %s during migration: %w", claimName, err)
			}

			entry := vmopv1util.VMEntry(cvi, vm.Name)
			if entry == nil || vmopv1util.NormalizeDiskMode(entry.DiskMode) != diskMode || entry.VolumeName != volSpec.Name {
				patch := ctrlclient.MergeFrom(cvi.DeepCopy())
				if entry == nil {
					cvi.Spec.VMs = append(cvi.Spec.VMs, cnsv1alpha1.VirtualMachineRef{
						VMName:         vm.Name,
						VMInstanceUUID: vm.Status.InstanceUUID,
						DiskMode:       diskMode,
						VolumeName:     volSpec.Name,
					})
				} else {
					for j := range cvi.Spec.VMs {
						if cvi.Spec.VMs[j].VMName == vm.Name {
							cvi.Spec.VMs[j].DiskMode = diskMode
							cvi.Spec.VMs[j].VolumeName = volSpec.Name
						}
					}
				}
				if err := r.Client.Patch(ctx, cvi, patch); err != nil {
					return fmt.Errorf("failed to write CsiVolumeInfo spec.vms entry for PVC %s during migration: %w",
						claimName, err)
				}
				ctx.Logger.Info("Wrote VM entry to CsiVolumeInfo spec.vms for migration",
					"pvc", claimName, "cvi", cvi.Name, "diskMode", diskMode, "volumeName", volSpec.Name)
			}

			// Re-read and assert the entry landed before touching BA.spec — the
			// entry-before-detach ordering (§4.1) that keeps CSI's PVC
			// pvc-volume-protection finalizer handoff gapless (D5, §21.4).
			confirmed := &cnsv1alpha1.CsiVolumeInfo{}
			if err := r.Client.Get(ctx, ctrlclient.ObjectKey{Namespace: cvi.Namespace, Name: cvi.Name}, confirmed); err != nil {
				return fmt.Errorf("failed to confirm CsiVolumeInfo entry for PVC %s during migration: %w", claimName, err)
			}
			confirmedEntry := vmopv1util.VMEntry(confirmed, vm.Name)
			if confirmedEntry == nil {
				return fmt.Errorf("CsiVolumeInfo entry for PVC %s did not persist during migration", claimName)
			}

			// Safe to release the disk from BA.spec now — its CVI entry exists,
			// regardless of whether CSI has finished acting on it yet.
			toRemove.Insert(volSpec.Name)

			if vmopv1util.IsDependentMode(confirmedEntry.DiskMode) {
				// Both clean (no annotation) and deferred (fcd-retained) count
				// as migrated (§4.4) — VMManaged is the only gate.
				if confirmed.Status.Ownership != cnsv1alpha1.OwnershipStateVMManaged {
					ctx.Logger.Info("Waiting for CSI to transfer ownership during migration",
						"pvc", claimName, "cvi", cvi.Name)
					allDone = false
				}
			}
			// Independent: entry-present is sufficient (§12.2) — already true.
		}
	}

	// Legacy-CnsNodeVmAttachment-tracked disks migrate the same way; the
	// mechanism-specific difference is the cleanup step below (CR deletion
	// in place of removal from BA.spec, migration §12).
	for i := range legacyToMigrate {
		vol := legacyToMigrate[i]
		if live := findVolumeSpecByName(vm, vol.Name); live != nil {
			// legacyToMigrate is a snapshot taken before convertVKSDiskModes
			// ran; re-read the current spec value so a VKS conversion above
			// is reflected in the CVI entry this loop writes.
			vol = *live
		}
		claimName := vol.PersistentVolumeClaim.ClaimName
		diskMode := vmopv1util.DiskModeForVolume(vol)

		cvi, err := vmopv1util.EnsureCVIForPVC(ctx, r.Client, vm.Namespace, claimName)
		if err != nil {
			if apierrors.IsNotFound(err) {
				ctx.Logger.Info("PVC or PV not resolvable — deferring migration for this disk",
					"pvc", claimName)
				allDone = false
				continue
			}
			return fmt.Errorf("failed to ensure CsiVolumeInfo for PVC %s during migration: %w", claimName, err)
		}

		entry := vmopv1util.VMEntry(cvi, vm.Name)
		if entry == nil || vmopv1util.NormalizeDiskMode(entry.DiskMode) != diskMode || entry.VolumeName != vol.Name {
			patch := ctrlclient.MergeFrom(cvi.DeepCopy())
			if entry == nil {
				cvi.Spec.VMs = append(cvi.Spec.VMs, cnsv1alpha1.VirtualMachineRef{
					VMName:         vm.Name,
					VMInstanceUUID: vm.Status.InstanceUUID,
					DiskMode:       diskMode,
					VolumeName:     vol.Name,
				})
			} else {
				for j := range cvi.Spec.VMs {
					if cvi.Spec.VMs[j].VMName == vm.Name {
						cvi.Spec.VMs[j].DiskMode = diskMode
						cvi.Spec.VMs[j].VolumeName = vol.Name
					}
				}
			}
			if err := r.Client.Patch(ctx, cvi, patch); err != nil {
				return fmt.Errorf("failed to write CsiVolumeInfo spec.vms entry for PVC %s during migration: %w",
					claimName, err)
			}
			ctx.Logger.Info("Wrote VM entry to CsiVolumeInfo spec.vms for migration (legacy-tracked disk)",
				"pvc", claimName, "cvi", cvi.Name, "diskMode", diskMode, "volumeName", vol.Name)
		}

		confirmed := &cnsv1alpha1.CsiVolumeInfo{}
		if err := r.Client.Get(ctx, ctrlclient.ObjectKey{Namespace: cvi.Namespace, Name: cvi.Name}, confirmed); err != nil {
			return fmt.Errorf("failed to confirm CsiVolumeInfo entry for PVC %s during migration: %w", claimName, err)
		}
		confirmedEntry := vmopv1util.VMEntry(confirmed, vm.Name)
		if confirmedEntry == nil {
			return fmt.Errorf("CsiVolumeInfo entry for PVC %s did not persist during migration", claimName)
		}

		if vmopv1util.IsDependentMode(confirmedEntry.DiskMode) {
			if confirmed.Status.Ownership != cnsv1alpha1.OwnershipStateVMManaged {
				ctx.Logger.Info("Waiting for CSI to transfer ownership during migration",
					"pvc", claimName, "cvi", cvi.Name)
				allDone = false
				continue
			}
		}

		// Safe to delete the legacy CnsNodeVmAttachment now — its CVI entry
		// exists and (for a dependent disk) has reached VMManaged, mirroring
		// the BA "remove from spec" step (migration §12).
		attachmentName := pkgutil.CNSAttachmentNameForVolume(vm.Name, vol.Name)
		if attachment, ok := legacyAttachments[attachmentName]; ok {
			if err := r.Client.Delete(ctx, &attachment); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("failed to delete legacy CnsNodeVmAttachment %s after migration: %w",
					attachment.Name, err)
			}
		}
	}

	if ba != nil && toRemove.Len() > 0 {
		patch := ctrlclient.MergeFrom(ba.DeepCopy())
		remaining := make([]cnsv1alpha1.VolumeSpec, 0, len(ba.Spec.Volumes))
		for _, vs := range ba.Spec.Volumes {
			if !toRemove.Has(vs.Name) {
				remaining = append(remaining, vs)
			}
		}
		ba.Spec.Volumes = remaining
		if err := r.Client.Patch(ctx, ba, patch); err != nil {
			return fmt.Errorf("failed to remove migrated volumes from CnsNodeVMBatchAttachment %s: %w", ba.Name, err)
		}
	}

	if !allDone {
		return pkgerr.RequeueError{
			After:   5 * time.Second,
			Message: "waiting for dependent disks to reach VMManaged during migration",
		}
	}

	return r.completeMigration(ctx, ba)
}

// completeMigration commits migration's Stage 2 (§4.4, §12.3, §12.5): the
// vm-owned-volumes annotation flip is the commit point and must be the last
// write before the BA is retired, so it is patched here directly rather than
// left to Reconcile's deferred patch of ctx.VM — that patch runs after this
// function returns, which would let the BA delete land first on a crash.
func (r *Reconciler) completeMigration(ctx *pkgctx.VolumeContext, ba *cnsv1alpha1.CnsNodeVMBatchAttachment) error {
	vm := ctx.VM

	vmPatch := ctrlclient.MergeFrom(vm.DeepCopy())
	metav1.SetMetaDataAnnotation(&vm.ObjectMeta, pkgconst.VMOwnedVolumesAnnotation, "true")
	if err := r.Client.Patch(ctx, vm, vmPatch); err != nil {
		return fmt.Errorf("failed to set vm-owned-volumes annotation after migration: %w", err)
	}
	ctx.Logger.Info("Migration complete; VM is now VM-owned")

	if ba == nil {
		return nil
	}

	if ba.Annotations[pkgconst.VMOwnedMigrationAnnotation] != pkgconst.VMOwnedMigrationComplete {
		baPatch := ctrlclient.MergeFrom(ba.DeepCopy())
		metav1.SetMetaDataAnnotation(&ba.ObjectMeta, pkgconst.VMOwnedMigrationAnnotation, pkgconst.VMOwnedMigrationComplete)
		if err := r.Client.Patch(ctx, ba, baPatch); err != nil {
			return fmt.Errorf("failed to mark CnsNodeVMBatchAttachment %s migration complete: %w", ba.Name, err)
		}
	}

	// vm-operator issues the Delete; CSI's BA controller observes it, clears
	// the tracked PVC pvc-protection finalizers and its own finalizer, and
	// the object is collected — the standard Kubernetes split (§12.5, §21.3).
	if err := r.Client.Delete(ctx, ba); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete CnsNodeVMBatchAttachment %s after migration: %w", ba.Name, err)
	}

	return nil
}

// vksDiskModeCandidate is one attached, non-machine-owned PVC disk on a VKS
// node VM that still needs converting to independent-persistent.
type vksDiskModeCandidate struct {
	volumeName          string
	controllerType      vmopv1.VirtualControllerType
	controllerBusNumber int32
	unitNumber          int32
}

// convertVKSDiskModes implements migration §4.5 for a VKS node VM: every
// currently-attached PVC disk that is NOT the node VM's own machine-lifecycle
// disk (i.e. its PVC's ownerReferences do not identify a VirtualMachine or
// VSphereMachine) — a Kubernetes workload volume CSI attached to the node
// from inside the guest cluster — is converted from dependent-persistent to
// independent-persistent in a single ReconfigVM_Task. Driven off
// vm.status.volumes so it covers a disk regardless of whether it is tracked
// via a CnsNodeVMBatchAttachment, a legacy CnsNodeVmAttachment, or already a
// CsiVolumeInfo. The vm.spec.volumes patch below is the only durable record
// of the conversion; the caller's subsequent per-disk migration loop must
// re-read disk mode from vm.spec.volumes rather than trust a BA/legacy
// snapshot taken before this function ran.
func (r *Reconciler) convertVKSDiskModes(ctx *pkgctx.VolumeContext, vm *vmopv1.VirtualMachine) error {
	var candidates []vksDiskModeCandidate

	for i := range vm.Status.Volumes {
		vs := &vm.Status.Volumes[i]
		if vs.Type != vmopv1.VolumeTypeManaged {
			continue
		}
		if vs.DiskMode == vmopv1.VolumeDiskModeIndependentPersistent {
			continue
		}

		specVol := findVolumeSpecByName(vm, vs.Name)
		if specVol == nil || specVol.PersistentVolumeClaim == nil {
			// No PVC to classify (volume removed concurrently, or not
			// PVC-backed) — nothing to convert.
			continue
		}

		pvc := &corev1.PersistentVolumeClaim{}
		if err := r.Client.Get(ctx,
			ctrlclient.ObjectKey{Namespace: vm.Namespace, Name: specVol.PersistentVolumeClaim.ClaimName},
			pvc); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("failed to get PVC %s to classify machine-ownership during VKS conversion: %w",
				specVol.PersistentVolumeClaim.ClaimName, err)
		}
		if vmopv1util.IsMachineOwnedPVC(pvc) {
			// The node VM's own disk — leave dependent.
			continue
		}

		if vs.ControllerBusNumber == nil || vs.UnitNumber == nil {
			return pkgerr.RequeueError{
				After: 5 * time.Second,
				Message: fmt.Sprintf(
					"waiting for device slot for volume %s before VKS disk-mode conversion", vs.Name),
			}
		}
		candidates = append(candidates, vksDiskModeCandidate{
			volumeName:          vs.Name,
			controllerType:      vs.ControllerType,
			controllerBusNumber: *vs.ControllerBusNumber,
			unitNumber:          *vs.UnitNumber,
		})
	}

	if len(candidates) == 0 {
		return nil
	}

	// The host refuses a disk-mode change on a VM with any snapshot — the
	// check is VM-level, so one snapshot anywhere blocks every disk on it.
	// The platform's own rejection carries no property path or device
	// index, so precheck explicitly rather than surface it (migration
	// §4.5). A violation stalls migration; it must not create a new state —
	// deferred-unregister is a dependent-disk concept only (§9).
	hasSnapshot, err := r.VMProvider.HasAnySnapshot(ctx, vm)
	if err != nil {
		return fmt.Errorf("failed to check for VM snapshots before VKS disk-mode conversion: %w", err)
	}
	if hasSnapshot {
		return pkgerr.RequeueError{
			After: 30 * time.Second,
			Message: fmt.Sprintf(
				"VM %s has a vSphere snapshot; VKS disk-mode conversion cannot proceed until it is removed",
				vm.Name),
		}
	}

	// One patch covering every candidate's spec.diskMode — a deliberate,
	// spec-mandated controller write (migration §4.5), the same kind of
	// exception to the controllers-don't-write-spec rule that
	// restoreVMSpecFromSnapshot takes for reverts.
	vmPatch := ctrlclient.MergeFrom(vm.DeepCopy())
	for _, c := range candidates {
		for j := range vm.Spec.Volumes {
			if vm.Spec.Volumes[j].Name == c.volumeName {
				vm.Spec.Volumes[j].DiskMode = vmopv1.VolumeDiskModeIndependentPersistent
			}
		}
	}
	if err := r.Client.Patch(ctx, vm, vmPatch); err != nil {
		return fmt.Errorf("failed to rewrite disk mode for %d volume(s) during VKS migration: %w",
			len(candidates), err)
	}

	slots := make([]providers.VolumeDiskModeSlot, 0, len(candidates))
	for _, c := range candidates {
		slots = append(slots, providers.VolumeDiskModeSlot{
			VolumeName:          c.volumeName,
			ControllerType:      c.controllerType,
			ControllerBusNumber: c.controllerBusNumber,
			UnitNumber:          c.unitNumber,
		})
	}
	if err := r.VMProvider.ConvertDisksToIndependentPersistent(ctx, vm, slots); err != nil {
		return fmt.Errorf("failed to reconfigure %d volume(s) to independent-persistent: %w", len(candidates), err)
	}

	for _, c := range candidates {
		if statusVol := findVolumeStatusByName(vm, c.volumeName); statusVol != nil {
			statusVol.DiskMode = vmopv1.VolumeDiskModeIndependentPersistent
		}
		ctx.Logger.Info("Converted VKS workload disk to independent-persistent", "volume", c.volumeName)
	}

	return nil
}

// findVolumeSpecByName returns the vm.spec.volumes entry with the given
// name, or nil if the volume has already been removed from spec (a
// concurrent detach racing migration).
func findVolumeSpecByName(vm *vmopv1.VirtualMachine, name string) *vmopv1.VirtualMachineVolume {
	for i := range vm.Spec.Volumes {
		if vm.Spec.Volumes[i].Name == name {
			return &vm.Spec.Volumes[i]
		}
	}
	return nil
}

// cnsDiskModeToCVIDiskMode converts a CnsNodeVMBatchAttachment VolumeSpec's
// DiskMode (BA's own lower-snake-case enum) to the CsiVolumeInfo DiskMode
// enum. The two types are named identically in places but come from
// different Go packages within this module; this is the migration-only
// bridge between them, since the BA's VolumeSpec is the sole record of a
// disk's current mode for a volume that has already left vm.spec.volumes
// (the very detach that triggered migration).
func cnsDiskModeToCVIDiskMode(dm cnsv1alpha1.DiskMode) cnsv1alpha1.CVIDiskMode {
	switch dm {
	case cnsv1alpha1.IndependentPersistent:
		return cnsv1alpha1.CVIDiskModeIndependentPersistent
	case cnsv1alpha1.DiskMode(cnsv1alpha1.IndependentNonPersistent):
		return cnsv1alpha1.CVIDiskModeIndependentNonPersistent
	case cnsv1alpha1.DiskMode(cnsv1alpha1.NonPersistent):
		return cnsv1alpha1.CVIDiskModeNonPersistent
	default:
		return cnsv1alpha1.CVIDiskModePersistent
	}
}
