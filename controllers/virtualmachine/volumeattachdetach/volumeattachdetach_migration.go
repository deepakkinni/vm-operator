// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package volumeattachdetach

import (
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha6"
	cnsv1alpha1 "github.com/vmware-tanzu/vm-operator/external/vsphere-csi-driver/api/v1alpha1"
	pkgconst "github.com/vmware-tanzu/vm-operator/pkg/constants"
	pkgctx "github.com/vmware-tanzu/vm-operator/pkg/context"
	pkgerr "github.com/vmware-tanzu/vm-operator/pkg/errors"
	kubeutil "github.com/vmware-tanzu/vm-operator/pkg/util/kube"
	vmopv1util "github.com/vmware-tanzu/vm-operator/pkg/util/vmopv1"
)

// isMigrationCandidate reports whether ctx.VM should be routed through
// reconcileMigration this reconcile, per migration spec §4 (Trigger Model).
// A VM is a candidate when the feature gate is on, it lacks the
// vm-owned-volumes annotation, and either the explicit trigger annotation is
// present or it has at least one PVC-backed volume — the lazy trigger, which
// in a level-triggered reconciler is indistinguishable from "an attach or
// detach happened" and is safe to re-evaluate on every reconcile because
// migration itself is idempotent (migration §17).
func isMigrationCandidate(vm *vmopv1.VirtualMachine) bool {
	if vmopv1util.HasVMOwnedVolumesAnnotation(vm) {
		return false
	}
	if metav1.HasAnnotation(vm.ObjectMeta, pkgconst.MigrateToVMOwnedAnnotation) {
		return true
	}
	for _, vol := range vm.Spec.Volumes {
		if vol.PersistentVolumeClaim != nil {
			return true
		}
	}
	return false
}

// reconcileMigration drives a brownfield VM's already-attached disks onto
// the CsiVolumeInfo path (migration §4, §7, §12), then flips
// vm-owned-volumes once every disk has landed there (§4.4). It returns
// pkgerr.RequeueError until migration completes, so the caller — ReconcileNormal
// — never processes the triggering attach/detach as a VM-owned workflow
// against a half-migrated VM (§4.1's ordering rule).
func (r *Reconciler) reconcileMigration(ctx *pkgctx.VolumeContext) error {
	vm := ctx.VM

	ba, err := r.getBatchAttachmentForVM(ctx)
	if err != nil {
		return fmt.Errorf("failed to get CnsNodeVMBatchAttachment for migration: %w", err)
	}

	if ba == nil {
		// No existing BA — nothing attached to migrate. The lazy trigger
		// fired on the VM's very first PVC, so there is nothing to freeze or
		// retire; go straight to Stage 2 so the triggering attach is
		// processed as VM-owned Workflow A on the next reconcile.
		ctx.Logger.Info("Migration candidate has no CnsNodeVMBatchAttachment; nothing to migrate")
		return r.completeMigration(ctx, nil)
	}

	if ba.Annotations[pkgconst.VMOwnedMigrationAnnotation] != pkgconst.VMOwnedMigrationInProgress {
		// Stage 1 — freeze (§12.1). Nothing else may proceed until this
		// patch is confirmed observed: if a disk left BA.spec before the
		// freeze landed, CSI would detach a live disk.
		patch := ctrlclient.MergeFrom(ba.DeepCopy())
		metav1.SetMetaDataAnnotation(&ba.ObjectMeta, pkgconst.VMOwnedMigrationAnnotation, pkgconst.VMOwnedMigrationInProgress)
		if err := r.Client.Patch(ctx, ba, patch); err != nil {
			return fmt.Errorf("failed to freeze CnsNodeVMBatchAttachment %s for migration: %w", ba.Name, err)
		}
		ctx.Logger.Info("Froze CnsNodeVMBatchAttachment for migration", "batchAttachment", ba.Name)
	}

	// VKS disk-mode conversion (V12, migration §4.5) precedes the general
	// per-disk loop below, so the loop reads the already-rewritten mode and
	// never hands CSI a spec.vms entry the device does not yet match.
	if kubeutil.HasCAPILabels(vm.Labels) {
		if err := r.convertVKSDiskModes(ctx, ba); err != nil {
			return err
		}
	}

	var (
		toRemove = sets.New[string]()
		allDone  = true
	)

	for i := range ba.Spec.Volumes {
		volSpec := ba.Spec.Volumes[i]
		claimName := volSpec.PersistentVolumeClaim.ClaimName
		diskMode := cnsDiskModeToCVIDiskMode(volSpec.PersistentVolumeClaim.DiskMode)

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

	if toRemove.Len() > 0 {
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

// convertVKSDiskModes implements migration §4.5 for a VKS node VM: every
// non-boot PVC disk on the BA — which, by construction, is every disk on the
// BA, since a boot disk has no PVC and is never tracked there — is converted
// from dependent-persistent to independent-persistent before the shared
// per-disk migration loop hands it to the CVI. Mutates ba.Spec.Volumes'
// in-memory DiskMode so the caller's loop sees the new mode without a
// re-fetch.
func (r *Reconciler) convertVKSDiskModes(ctx *pkgctx.VolumeContext, ba *cnsv1alpha1.CnsNodeVMBatchAttachment) error {
	vm := ctx.VM

	needsConversion := false
	for _, vs := range ba.Spec.Volumes {
		if vs.PersistentVolumeClaim.DiskMode != cnsv1alpha1.IndependentPersistent {
			needsConversion = true
			break
		}
	}
	if !needsConversion {
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

	for i := range ba.Spec.Volumes {
		vs := &ba.Spec.Volumes[i]
		if vs.PersistentVolumeClaim.DiskMode == cnsv1alpha1.IndependentPersistent {
			continue
		}

		if specVol := findVolumeSpecByName(vm, vs.Name); specVol != nil &&
			specVol.DiskMode != vmopv1.VolumeDiskModeIndependentPersistent {
			// Deliberate, spec-mandated controller write to spec.volumes
			// (migration §4.5) — the same kind of exception to the
			// controllers-don't-write-spec rule that restoreVMSpecFromSnapshot
			// takes for reverts. Nothing else writes diskMode on a node VM.
			vmPatch := ctrlclient.MergeFrom(vm.DeepCopy())
			for j := range vm.Spec.Volumes {
				if vm.Spec.Volumes[j].Name == vs.Name {
					vm.Spec.Volumes[j].DiskMode = vmopv1.VolumeDiskModeIndependentPersistent
				}
			}
			if err := r.Client.Patch(ctx, vm, vmPatch); err != nil {
				return fmt.Errorf("failed to rewrite disk mode for volume %s during VKS migration: %w", vs.Name, err)
			}
		}

		statusVol := findVolumeStatusByName(vm, vs.Name)
		if statusVol == nil || statusVol.ControllerBusNumber == nil || statusVol.UnitNumber == nil {
			return pkgerr.RequeueError{
				After: 5 * time.Second,
				Message: fmt.Sprintf(
					"waiting for device slot for volume %s before VKS disk-mode conversion", vs.Name),
			}
		}

		if err := r.VMProvider.ConvertDiskToIndependentPersistent(
			ctx, vm, statusVol.ControllerType, *statusVol.ControllerBusNumber, *statusVol.UnitNumber); err != nil {
			return fmt.Errorf("failed to reconfigure volume %s to independent-persistent: %w", vs.Name, err)
		}
		ctx.Logger.Info("Converted VKS disk to independent-persistent", "volume", vs.Name)

		vs.PersistentVolumeClaim.DiskMode = cnsv1alpha1.IndependentPersistent
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
