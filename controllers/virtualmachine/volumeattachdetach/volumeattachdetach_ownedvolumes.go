// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package volumeattachdetach

import (
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha6"
	cnsv1alpha1 "github.com/vmware-tanzu/vm-operator/external/vsphere-csi-driver/api/v1alpha1"
	pkgconst "github.com/vmware-tanzu/vm-operator/pkg/constants"
	pkgctx "github.com/vmware-tanzu/vm-operator/pkg/context"
	pkgerr "github.com/vmware-tanzu/vm-operator/pkg/errors"
	"github.com/vmware-tanzu/vm-operator/pkg/providers"
	kubeutil "github.com/vmware-tanzu/vm-operator/pkg/util/kube"
	vmopv1util "github.com/vmware-tanzu/vm-operator/pkg/util/vmopv1"
	"github.com/vmware-tanzu/vm-operator/pkg/volumes/owned"
)

// ErrDiskPathRefreshExhausted means a diskPath refresh has already completed
// for the failing path and returned that same path, so the attach cannot be
// made to succeed by asking again.
//
// This is a permanent condition, deliberately distinguished from the ordinary
// stale-path case: the latter requeues and retries, while this must surface so
// an operator sees it. Retrying costs a ReconfigVM_Task against vCenter every
// few seconds and would never converge.
var ErrDiskPathRefreshExhausted = errors.New("diskPath refresh did not yield a new path")

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
	fcdID    string // set iff the disk is a still-registered, linked-clone FCD
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
// On a VKS node VM, a volume whose PVC is not owned by the node's own
// VirtualMachine or VSphereMachine is a Kubernetes workload volume attached
// from inside the guest cluster; its spec.diskMode is forced to
// independent-persistent before its CsiVolumeInfo entry is written, so its
// lifecycle is never entangled with the node VM's own disk-unregister/
// ownership semantics.
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
	isVKS := kubeutil.HasCAPILabels(vm.Labels)

	for _, plan := range owned.ClassifyVolumes(vm) {
		// Already attached — nothing to do.
		if _, ok := statusVolumeNames[plan.VolumeName]; ok {
			continue
		}

		claimName := plan.ClaimName

		// On a VKS node VM, a PVC not owned by the node's own VirtualMachine
		// or VSphereMachine is a Kubernetes workload volume CSI attached
		// from inside the guest cluster — its lifecycle must never be
		// entangled with the node VM's own disk-unregister/ownership
		// semantics, so it is forced to independent-persistent before its
		// CsiVolumeInfo entry is written.
		var pvc *corev1.PersistentVolumeClaim
		if isVKS {
			pvc = &corev1.PersistentVolumeClaim{}
			if err := r.Client.Get(ctx, ctrlclient.ObjectKey{Namespace: vm.Namespace, Name: claimName}, pvc); err != nil {
				if apierrors.IsNotFound(err) {
					ctx.Logger.Info("PVC not resolvable — skipping vm-owned volumes path for now",
						"pvc", claimName)
					continue
				}
				return fmt.Errorf("failed to get PVC %s to classify machine-ownership: %w", claimName, err)
			}
			if !vmopv1util.IsMachineOwnedPVC(pvc) && plan.RawDiskMode != vmopv1.VolumeDiskModeIndependentPersistent {
				if err := r.forceIndependentPersistentDiskMode(ctx, vm, plan.VolumeName); err != nil {
					return err
				}
				plan.RawDiskMode = vmopv1.VolumeDiskModeIndependentPersistent
				plan.DiskMode = cnsv1alpha1.CVIDiskModeIndependentPersistent
				plan.Dependent = false
			}
		}

		// Writing this entry is the only PVC-usage bookkeeping vm-operator
		// does here. The `cns.vmware.com/usedby-vm-<uuid>` label on the PVC
		// is CSI's alone to maintain, keyed on this same spec.vms becoming
		// non-empty (attach/detach §13.8) — do not also stamp it here.
		//
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

		// A previously-requested diskPath refresh (see requestDiskPathRefresh)
		// is still in flight: CSI clears this annotation only after it has
		// already replaced spec.diskPath with the freshly-resolved value, so
		// its presence means the field on this read cannot be trusted yet —
		// for either disk mode. Wait rather than retry with what is likely
		// still the same stale value that caused the refresh request.
		if _, refreshPending := cvi.Annotations[cnsv1alpha1.DiskPathRefreshRequestedAnnotation]; refreshPending {
			ctx.Logger.Info("Waiting for CSI to refresh a stale diskPath",
				"pvc", claimName, "cvi", cvi.Name)
			needRequeue = true
			continue
		}

		// The refresh has finished (no annotation) and produced a path
		// different from the one recorded as stale, so the record is spent:
		// drop it, allowing a future staleness at this new path its own
		// refresh attempt. Doing this here rather than on the attach path
		// costs nothing — the CsiVolumeInfo is already in hand.
		if prev, ok := cvi.Annotations[pkgconst.StaleDiskPathAnnotationKey]; ok && prev != cvi.Spec.DiskPath {
			patch := ctrlclient.MergeFrom(cvi.DeepCopy())
			delete(cvi.Annotations, pkgconst.StaleDiskPathAnnotationKey)
			if err := r.Client.Patch(ctx, cvi, patch); err != nil {
				return fmt.Errorf("failed to clear %s on CsiVolumeInfo %s: %w",
					pkgconst.StaleDiskPathAnnotationKey, cvi.Name, err)
			}
			ctx.Logger.Info("Cleared spent stale-diskPath record after a successful refresh",
				"pvc", claimName, "cvi", cvi.Name, "previous", prev, "current", cvi.Spec.DiskPath)
		}

		// Two readiness classes (attach/detach §7.3 A.4/A.5): a dependent
		// disk requires CSI's ownership-transfer handshake to finish — the
		// green signal — before its FCD is vm-operator's to attach. An
		// independent disk is never transferred: CSI stays CSIManaged and
		// only needs to publish spec.diskPath, so it is ready as soon as
		// that is present. Waiting on IsGreenSignal (Ownership==VMManaged)
		// for an independent disk would wait forever, since CSI never
		// flips ownership for one.
		if plan.Dependent {
			if !vmopv1util.IsGreenSignal(cvi) {
				ctx.Logger.Info("Waiting for CSI to unregister volume (green signal not yet present)",
					"pvc", claimName, "cvi", cvi.Name)
				needRequeue = true
				continue
			}
		} else if cvi.Spec.DiskPath == "" {
			ctx.Logger.Info("Waiting for CSI to provision diskPath for independent volume",
				"pvc", claimName, "cvi", cvi.Name)
			needRequeue = true
			continue
		}

		diskPath := cvi.Spec.DiskPath
		if diskPath == "" {
			return fmt.Errorf("CsiVolumeInfo %s has empty diskPath after readiness check", cvi.Name)
		}

		rd := readyDependentDisk{
			plan:     plan,
			diskPath: diskPath,
			diskUUID: cvi.Spec.DiskUUID,
		}
		if vmopv1util.IsFcdRetained(cvi) {
			// vDiskId is supplied only for a linked-clone retained FCD —
			// the one case vpxd's LinkedCloneFcdAttachPrechecks needs the
			// FCD identity for (attach/detach §7.1.5). Setting it
			// unconditionally for every fcd-retained disk also routes the
			// reconfigure through vpxd's unrelated VSLM
			// reconfigure-precheck callback, which mishandles the
			// datastore path for a snapshot-blocked retained FCD
			// ("Invalid datastore path"), so it must stay scoped to the
			// linked-clone case. CBT-per-disk does not apply here: §7.1.4
			// reserves that directive for independent disks, and the
			// platform's default already produces the correct row for a
			// dependent fcd-retained disk.
			if pvc == nil {
				pvc = &corev1.PersistentVolumeClaim{}
				if err := r.Client.Get(ctx, ctrlclient.ObjectKey{Namespace: vm.Namespace, Name: claimName}, pvc); err != nil {
					return fmt.Errorf("failed to get PVC %s to check linked-clone annotation: %w", claimName, err)
				}
			}
			if vmopv1util.IsLinkedClonePVC(pvc) {
				rd.fcdID = cvi.Spec.VolumeID
			}
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
		if stale, ok := pkgerr.AsStaleDiskPathError(err); ok {
			// The disk was relocated (e.g. storage vMotion) after CSI last
			// resolved its CsiVolumeInfo.spec.diskPath. Request a refresh
			// per affected volume rather than surfacing a hard failure —
			// this is expected-if-rare, not a bug, and CSI's
			// csivolumeinfo controller knows how to re-resolve it for
			// either disk mode (see the annotation's doc comment).
			for _, volumeName := range stale.VolumeNames {
				rd, ok := readyDiskByVolumeName(ready, volumeName)
				if !ok {
					continue
				}
				// rd.diskPath is the value this reconcile actually tried, read
				// from spec.diskPath before the attach — which is what makes it
				// the right thing to record as "already refreshed for".
				if reqErr := r.requestDiskPathRefresh(ctx, vm, rd.plan.ClaimName, rd.diskPath); reqErr != nil {
					if errors.Is(reqErr, ErrDiskPathRefreshExhausted) {
						// Not retryable: surface it rather than requeueing into
						// a ReconfigVM_Task loop that cannot converge.
						return fmt.Errorf("cannot attach volume %q: %w", volumeName, reqErr)
					}
					return fmt.Errorf("failed to request diskPath refresh for volume %q: %w", volumeName, reqErr)
				}
			}
			return pkgerr.RequeueError{
				After: 5 * time.Second,
				Message: fmt.Sprintf(
					"stale diskPath %q for %d volume(s); requested a CSI refresh", stale.Path, len(stale.VolumeNames)),
			}
		}
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
			DiskMode:            rd.plan.RawDiskMode,
			ControllerType:      p.ControllerType,
			ControllerBusNumber: &p.ControllerBusNumber,
			UnitNumber:          &p.UnitNumber,
		})
	}

	return nil
}

// readyDiskByVolumeName finds the readyDependentDisk for the given
// vm.spec.volumes[*].name, or false if none matches.
func readyDiskByVolumeName(ready []readyDependentDisk, volumeName string) (readyDependentDisk, bool) {
	for _, rd := range ready {
		if rd.plan.VolumeName == volumeName {
			return rd, true
		}
	}
	return readyDependentDisk{}, false
}

// requestDiskPathRefresh sets DiskPathRefreshRequestedAnnotation on the
// PVC's CsiVolumeInfo, asking CSI's csivolumeinfo controller to re-resolve
// spec.diskPath from a live query. vm-operator never clears or rewrites
// spec.diskPath itself here — for a dependent (VMManaged) volume that field
// being non-empty is a durable invariant vm-operator's own attach path also
// relies on, so only CSI may touch it; this annotation is the signal.
func (r *Reconciler) requestDiskPathRefresh(
	ctx *pkgctx.VolumeContext, vm *vmopv1.VirtualMachine, claimName, stalePath string) error {

	cvi, err := vmopv1util.GetCVIForPVC(ctx, r.Client, vm.Namespace, claimName)
	if err != nil {
		return fmt.Errorf("failed to get CsiVolumeInfo for PVC %s: %w", claimName, err)
	}
	if _, alreadyRequested := cvi.Annotations[cnsv1alpha1.DiskPathRefreshRequestedAnnotation]; alreadyRequested {
		return nil
	}

	// A refresh has already run for this exact path and did not change it, so
	// requesting another cannot help — CSI resolves a dependent volume's path
	// from the VM's device list, which cannot answer while the disk is
	// detached, and it is detached precisely because the attach failed. Fail
	// permanently instead of spinning.
	if prev, ok := cvi.Annotations[pkgconst.StaleDiskPathAnnotationKey]; ok && prev == stalePath {
		return fmt.Errorf("%w: diskPath %q for PVC %s", ErrDiskPathRefreshExhausted, stalePath, claimName)
	}

	patch := ctrlclient.MergeFrom(cvi.DeepCopy())
	metav1.SetMetaDataAnnotation(&cvi.ObjectMeta, cnsv1alpha1.DiskPathRefreshRequestedAnnotation, "true")
	metav1.SetMetaDataAnnotation(&cvi.ObjectMeta, pkgconst.StaleDiskPathAnnotationKey, stalePath)
	if err := r.Client.Patch(ctx, cvi, patch); err != nil {
		return fmt.Errorf("failed to set %s on CsiVolumeInfo %s: %w",
			cnsv1alpha1.DiskPathRefreshRequestedAnnotation, cvi.Name, err)
	}
	ctx.Logger.Info("Requested diskPath refresh for stale-attach PVC",
		"pvc", claimName, "cvi", cvi.Name, "stalePath", stalePath)
	return nil
}

// forceIndependentPersistentDiskMode patches vm.Spec.Volumes[i].DiskMode to
// IndependentPersistent for the named volume. This is a controller write to
// spec, the same class of exception convertVKSDiskModes takes (migration
// §4.5) — required so a workload PVC attached to a VKS node VM is never left
// in dependent mode, which would let CSI attempt an ownership-transfer
// unregister on a PVC vm-operator does not own.
func (r *Reconciler) forceIndependentPersistentDiskMode(
	ctx *pkgctx.VolumeContext, vm *vmopv1.VirtualMachine, volumeName string) error {

	patch := ctrlclient.MergeFrom(vm.DeepCopy())
	found := false
	for i := range vm.Spec.Volumes {
		if vm.Spec.Volumes[i].Name == volumeName {
			vm.Spec.Volumes[i].DiskMode = vmopv1.VolumeDiskModeIndependentPersistent
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("volume %s not found in spec.volumes while forcing disk mode", volumeName)
	}
	if err := r.Client.Patch(ctx, vm, patch); err != nil {
		return fmt.Errorf("failed to force independent-persistent disk mode for volume %s: %w", volumeName, err)
	}
	ctx.Logger.Info("Forced independent-persistent disk mode for workload PVC on VKS node",
		"volume", volumeName)
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
//
// The converse also holds: a status entry can name a slot the live VM no
// longer fills. A snapshot revert removes any disk added after the snapshot
// was taken, but leaves the status entry behind, because status.volumes for
// managed volumes tracks attachment state rather than mirroring hardware —
// updateVolumeStatus prunes only Classic entries, and updateVMVolumeStatus
// preserves CVI-owned ones. Both provider slot calls therefore report
// providers.ErrDiskNotFoundAtSlot as an expected outcome, not an error, and
// the volume takes the same disk-not-on-VM path. Erroring out instead would
// deadlock: the stale entry blocks the detach, and only the detach clears
// the stale entry.
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
			if !errors.Is(err, providers.ErrDiskNotFoundAtSlot) {
				return fmt.Errorf("failed to detach independent disk for CsiVolumeInfo %s: %w", cvi.Name, err)
			}
			ctx.Logger.Info("Independent VM-owned disk already absent from VM; treating as detached",
				"cvi", cvi.Name, "pvc", cvi.Spec.PVCName)
		} else {
			ctx.Logger.Info("Removed independent VM-owned disk from VM",
				"cvi", cvi.Name, "pvc", cvi.Spec.PVCName)
		}

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
		if !errors.Is(err, providers.ErrDiskNotFoundAtSlot) {
			return fmt.Errorf("failed to read live disk path for CsiVolumeInfo %s: %w", cvi.Name, err)
		}

		// The slot the status entry names holds no disk, so the device is
		// already off the VM: a snapshot revert dropped a disk added after
		// the snapshot was taken, leaving the status entry that named its
		// slot behind. Keep spec.diskPath as it stands — the live VM no
		// longer carries a path to refresh it from — and issue no
		// ReconfigVM. Falling through to removeCVIEntryIfNotRetained both
		// clears the stale status entry and, when no snapshot still retains
		// the disk, releases the CVI entry so CSI can re-register the
		// volume.
		ctx.Logger.Info("VM-owned disk already absent from VM; treating as detached",
			"cvi", cvi.Name, "pvc", cvi.Spec.PVCName,
			"controllerType", statusEntry.ControllerType,
			"controllerBusNumber", *statusEntry.ControllerBusNumber,
			"unitNumber", *statusEntry.UnitNumber)

		return r.removeCVIEntryIfNotRetained(ctx, cvi, statusEntry, dependent)
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
		// Not-found is tolerated here too: the disk can go away between the
		// path read above and this remove.
		if !errors.Is(err, providers.ErrDiskNotFoundAtSlot) {
			return fmt.Errorf("failed to detach disk for CsiVolumeInfo %s: %w", cvi.Name, err)
		}
		ctx.Logger.Info("VM-owned disk went absent before detach; treating as detached",
			"cvi", cvi.Name, "pvc", cvi.Spec.PVCName)
	} else {
		ctx.Logger.Info("Removed VM-owned disk from VM", "cvi", cvi.Name, "pvc", cvi.Spec.PVCName)
	}

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
