// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package vmopv1

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha6"
	cnsv1alpha1 "github.com/vmware-tanzu/vm-operator/external/vsphere-csi-driver/api/v1alpha1"
	pkgconst "github.com/vmware-tanzu/vm-operator/pkg/constants"
	"github.com/vmware-tanzu/vm-operator/pkg/util/ptr"
)

const (
	// CVIVMInstanceUUIDIndexKey is the field index registered on
	// CsiVolumeInfo.spec.vms[*].vmInstanceUUID. It is the primary key for
	// finding every CsiVolumeInfo that references a given VM without an
	// unbounded namespace scan (attach/detach §13.5.1;
	// implementation-rules §7).
	CVIVMInstanceUUIDIndexKey = "spec.vms.vmInstanceUUID"

	// CVIVMNameIndexKey is the field index registered on
	// CsiVolumeInfo.spec.vms[*].vmName. It is the fallback key for entries
	// written before vmInstanceUUID was observed. VM names are not unique
	// cluster-wide, so callers must still filter the result by
	// spec.pvcNamespace == the VM's namespace.
	CVIVMNameIndexKey = "spec.vms.vmName"
)

// IndexCVIByVMInstanceUUID is the index function for CVIVMInstanceUUIDIndexKey.
func IndexCVIByVMInstanceUUID(obj ctrlclient.Object) []string {
	cvi, ok := obj.(*cnsv1alpha1.CsiVolumeInfo)
	if !ok {
		return nil
	}
	ids := make([]string, 0, len(cvi.Spec.VMs))
	for _, e := range cvi.Spec.VMs {
		if e.VMInstanceUUID != "" {
			ids = append(ids, e.VMInstanceUUID)
		}
	}
	return ids
}

// IndexCVIByVMName is the index function for CVIVMNameIndexKey.
func IndexCVIByVMName(obj ctrlclient.Object) []string {
	cvi, ok := obj.(*cnsv1alpha1.CsiVolumeInfo)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(cvi.Spec.VMs))
	for _, e := range cvi.Spec.VMs {
		if e.VMName != "" {
			names = append(names, e.VMName)
		}
	}
	return names
}

// HasVMOwnedVolumesAnnotation reports whether the VM is a VM-owned-volumes VM that uses the
// CsiVolumeInfo-based volume ownership path.
func HasVMOwnedVolumesAnnotation(vm *vmopv1.VirtualMachine) bool {
	return metav1.HasAnnotation(vm.ObjectMeta, pkgconst.VMOwnedVolumesAnnotation)
}

// CVINameForVolumeID returns the deterministic CsiVolumeInfo CR name for the
// volume identified by the given CNS volume ID.
func CVINameForVolumeID(volumeID string) string {
	return cnsv1alpha1.CVINamePrefix + volumeID
}

// resolvePVCVolume resolves the bound PV and CNS volume ID for the given
// PVC. Returns an apierrors.IsNotFound-compatible error if the PVC is not
// found, the PV it names does not exist, or (wrapped, not IsNotFound) if the
// PVC is unbound or the PV has no CSI volume handle.
func resolvePVCVolume(
	ctx context.Context,
	c ctrlclient.Client,
	pvcNamespace, pvcName string) (pvc *corev1.PersistentVolumeClaim, pv *corev1.PersistentVolume, volumeID string, err error) {

	pvc = &corev1.PersistentVolumeClaim{}
	if err := c.Get(ctx, ctrlclient.ObjectKey{
		Namespace: pvcNamespace,
		Name:      pvcName,
	}, pvc); err != nil {
		return nil, nil, "", fmt.Errorf("failed to get PVC %s/%s: %w", pvcNamespace, pvcName, err)
	}

	pvName := pvc.Spec.VolumeName
	if pvName == "" {
		return nil, nil, "", fmt.Errorf("PVC %s/%s is not yet bound to a PV", pvcNamespace, pvcName)
	}

	pv = &corev1.PersistentVolume{}
	if err := c.Get(ctx, ctrlclient.ObjectKey{Name: pvName}, pv); err != nil {
		return nil, nil, "", fmt.Errorf("failed to get PV %s for PVC %s/%s: %w", pvName, pvcNamespace, pvcName, err)
	}

	if pv.Spec.CSI == nil {
		return nil, nil, "", fmt.Errorf("PV %s does not have a CSI source", pvName)
	}
	volumeID = pv.Spec.CSI.VolumeHandle
	if volumeID == "" {
		return nil, nil, "", fmt.Errorf("PV %s has an empty CSI volumeHandle", pvName)
	}

	return pvc, pv, volumeID, nil
}

// GetCVIForPVC resolves the CsiVolumeInfo for the given PVC by looking up the
// PV from the PVC's spec.volumeName and extracting the volumeHandle. Returns
// an apierrors.IsNotFound-compatible error if the PVC, PV, or CVI is not found.
func GetCVIForPVC(
	ctx context.Context,
	c ctrlclient.Client,
	pvcNamespace, pvcName string) (*cnsv1alpha1.CsiVolumeInfo, error) {

	_, _, volumeID, err := resolvePVCVolume(ctx, c, pvcNamespace, pvcName)
	if err != nil {
		return nil, err
	}

	cvi := &cnsv1alpha1.CsiVolumeInfo{}
	if err := c.Get(ctx, ctrlclient.ObjectKey{
		Namespace: cnsv1alpha1.CVINamespace,
		Name:      CVINameForVolumeID(volumeID),
	}, cvi); err != nil {
		return nil, fmt.Errorf("failed to get CsiVolumeInfo for volume %s: %w", volumeID, err)
	}

	return cvi, nil
}

// EnsureCVIForPVC resolves the CsiVolumeInfo for the given PVC, creating it
// if it does not yet exist. Unlike GetCVIForPVC, a missing CVI is not an
// error: on a VM-owned VM a missing CVI is an anomaly to repair, not a
// brownfield PVC to skip (attach/detach §4.1.2, §13.1). vm-operator sets the
// PV's ownerRef on the CVI it creates — a cluster-scoped owner with a
// namespaced dependent, which Kubernetes permits — so the object is
// collectible from the moment it exists, closing the window before CSI's
// own reconcile backfills the same reference.
func EnsureCVIForPVC(
	ctx context.Context,
	c ctrlclient.Client,
	pvcNamespace, pvcName string) (*cnsv1alpha1.CsiVolumeInfo, error) {

	pvc, pv, volumeID, err := resolvePVCVolume(ctx, c, pvcNamespace, pvcName)
	if err != nil {
		return nil, err
	}

	cviKey := ctrlclient.ObjectKey{
		Namespace: cnsv1alpha1.CVINamespace,
		Name:      CVINameForVolumeID(volumeID),
	}

	cvi := &cnsv1alpha1.CsiVolumeInfo{}
	err = c.Get(ctx, cviKey, cvi)
	switch {
	case err == nil:
		return cvi, nil
	case !apierrors.IsNotFound(err):
		return nil, fmt.Errorf("failed to get CsiVolumeInfo for volume %s: %w", volumeID, err)
	}

	cvi = &cnsv1alpha1.CsiVolumeInfo{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cviKey.Name,
			Namespace: cviKey.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         corev1.SchemeGroupVersion.String(),
					Kind:               "PersistentVolume",
					Name:               pv.Name,
					UID:                pv.UID,
					Controller:         ptr.To(true),
					BlockOwnerDeletion: ptr.To(true),
				},
			},
		},
		Spec: cnsv1alpha1.CsiVolumeInfoSpec{
			VolumeID:     volumeID,
			PVCName:      pvcName,
			PVCNamespace: pvcNamespace,
			PVName:       pvc.Spec.VolumeName,
		},
	}
	if err := c.Create(ctx, cvi); err != nil {
		if apierrors.IsAlreadyExists(err) {
			if getErr := c.Get(ctx, cviKey, cvi); getErr != nil {
				return nil, fmt.Errorf(
					"failed to get CsiVolumeInfo for volume %s after AlreadyExists: %w", volumeID, getErr)
			}
			return cvi, nil
		}
		return nil, fmt.Errorf("failed to create CsiVolumeInfo for volume %s: %w", volumeID, err)
	}

	return cvi, nil
}

// ListCVIsForVM returns every CsiVolumeInfo in the CSI namespace with a
// spec.vms entry for the given VM, bounded by the matching-VM population —
// never a full-namespace scan (implementation-rules §7). It queries the
// vmInstanceUUID index (the primary key, attach/detach §13.5.1) and, for
// entries written before that field was observed, falls back to the vmName
// index, filtered by spec.pvcNamespace to disambiguate VMs that share a name
// across namespaces.
func ListCVIsForVM(
	ctx context.Context,
	c ctrlclient.Client,
	vm *vmopv1.VirtualMachine) ([]cnsv1alpha1.CsiVolumeInfo, error) {

	byName := make(map[string]cnsv1alpha1.CsiVolumeInfo)
	collect := func(list *cnsv1alpha1.CsiVolumeInfoList) {
		for _, cvi := range list.Items {
			if cvi.Spec.PVCNamespace != vm.Namespace {
				continue
			}
			byName[cvi.Name] = cvi
		}
	}

	if vm.Status.InstanceUUID != "" {
		list := &cnsv1alpha1.CsiVolumeInfoList{}
		if err := c.List(ctx, list,
			ctrlclient.InNamespace(cnsv1alpha1.CVINamespace),
			ctrlclient.MatchingFields{CVIVMInstanceUUIDIndexKey: vm.Status.InstanceUUID},
		); err != nil {
			return nil, fmt.Errorf("failed to list CsiVolumeInfo by vmInstanceUUID: %w", err)
		}
		collect(list)
	}

	nameList := &cnsv1alpha1.CsiVolumeInfoList{}
	if err := c.List(ctx, nameList,
		ctrlclient.InNamespace(cnsv1alpha1.CVINamespace),
		ctrlclient.MatchingFields{CVIVMNameIndexKey: vm.Name},
	); err != nil {
		return nil, fmt.Errorf("failed to list CsiVolumeInfo by vmName: %w", err)
	}
	collect(nameList)

	result := make([]cnsv1alpha1.CsiVolumeInfo, 0, len(byName))
	for _, cvi := range byName {
		result = append(result, cvi)
	}
	return result, nil
}

// DiskModeForVolume maps vm.spec.volumes[*].diskMode to the CsiVolumeInfo
// DiskMode enum, treating an empty value as Persistent, matching the
// vm.spec default (attach/detach §4.1.3).
func DiskModeForVolume(vol vmopv1.VirtualMachineVolume) cnsv1alpha1.CVIDiskMode {
	switch vol.DiskMode {
	case vmopv1.VolumeDiskModeIndependentPersistent:
		return cnsv1alpha1.CVIDiskModeIndependentPersistent
	case vmopv1.VolumeDiskModeIndependentNonPersistent:
		return cnsv1alpha1.CVIDiskModeIndependentNonPersistent
	case vmopv1.VolumeDiskModeNonPersistent:
		return cnsv1alpha1.CVIDiskModeNonPersistent
	case vmopv1.VolumeDiskModePersistent, "":
		return cnsv1alpha1.CVIDiskModePersistent
	default:
		return cnsv1alpha1.CVIDiskModePersistent
	}
}

// IsDependentMode reports whether the given CsiVolumeInfo disk mode is the
// dependent mode (ownership transfer via best-effort unregister). Every
// other mode is independent: the FCD stays registered and CSIManaged.
func IsDependentMode(dm cnsv1alpha1.CVIDiskMode) bool {
	return dm == "" || dm == cnsv1alpha1.CVIDiskModePersistent
}

// NormalizeDiskMode maps an empty CsiVolumeInfo disk mode to Persistent,
// matching the vm.spec default (VirtualMachineRef.DiskMode's documented
// convention). Use this before comparing an existing entry's DiskMode
// against a freshly computed one — otherwise an entry written before this
// field existed compares as different from the volume it already matches.
func NormalizeDiskMode(dm cnsv1alpha1.CVIDiskMode) cnsv1alpha1.CVIDiskMode {
	if dm == "" {
		return cnsv1alpha1.CVIDiskModePersistent
	}
	return dm
}

// IsFcdRetained reports whether the CsiVolumeInfo carries the fcd-retained
// annotation: a VMManaged volume whose FCD could not be unregistered because
// an in-place unregister was blocked (attach/detach §7.3 A.5).
func IsFcdRetained(cvi *cnsv1alpha1.CsiVolumeInfo) bool {
	return metav1.HasAnnotation(cvi.ObjectMeta, cnsv1alpha1.FcdRetainedAnnotation)
}

// IsLinkedClonePVC reports whether the PVC is a linked-clone
// ("fast provisioning") volume. vDiskId is supplied on the attach
// device-add only for this case: it is the one still-FCD scenario vpxd's
// LinkedCloneFcdAttachPrechecks needs the FCD identity for (attach/detach
// §7.1.1, §7.1.5). Setting it for any other still-FCD disk (CBT- or
// snapshot-blocked fcd-retained) buys nothing and additionally routes the
// reconfigure through vpxd's unrelated VSLM reconfigure-precheck callback.
func IsLinkedClonePVC(pvc *corev1.PersistentVolumeClaim) bool {
	return pvc.Annotations[pkgconst.PVCFastProvisioningAnnotation] == "true"
}

// machineOwnedPVCKinds are the ownerReference kinds that identify a PVC as a
// VKS node VM's own machine-lifecycle disk, as opposed to a Kubernetes
// workload volume CSI attached to the node from inside the guest cluster.
// Matched on Kind only: a VSphereMachine's exact apiVersion/group is not
// pinned here.
var machineOwnedPVCKinds = map[string]struct{}{
	"VirtualMachine": {}, // vmoperator.vmware.com/v1alpha6
	"VSphereMachine": {}, // vmware.infrastructure.cluster.x-k8s.io/v1beta2
}

// IsMachineOwnedPVC reports whether the PVC's ownerReferences identify it as
// a VKS node VM's own machine-lifecycle disk (owned by a VirtualMachine or
// VSphereMachine), as opposed to a Kubernetes workload volume attached to
// the node from inside the guest cluster. A PVC with no owner references, or
// owners of any other kind, is not machine-owned: on a VKS node VM this is
// the signal that the volume must be forced to
// VolumeDiskModeIndependentPersistent, so a workload PVC's lifecycle can
// never be entangled with the node VM's own disk-unregister/ownership
// semantics.
func IsMachineOwnedPVC(pvc *corev1.PersistentVolumeClaim) bool {
	for _, ref := range pvc.OwnerReferences {
		if _, ok := machineOwnedPVCKinds[ref.Kind]; ok {
			return true
		}
	}
	return false
}

// IsGreenSignal reports whether the CsiVolumeInfo status has the green signal
// that permits vm-operator to add the disk to the VM.
// Green signal = status.ownership==VMManaged && status.phase==Succeeded
// && status.observedGeneration >= metadata.generation.
//
// This must stay independent of fcd-retained: gating it on the annotation's
// absence would deadlock every deferred volume, which is VMManaged and
// Succeeded by design but never sheds the annotation (§7.3 A.5).
func IsGreenSignal(cvi *cnsv1alpha1.CsiVolumeInfo) bool {
	return cvi.Status.Ownership == cnsv1alpha1.OwnershipStateVMManaged &&
		cvi.Status.Phase == cnsv1alpha1.PhaseSucceeded &&
		cvi.Status.ObservedGeneration >= cvi.Generation
}

// VMEntry returns the spec.vms entry for the given vmName, or nil if none
// exists.
func VMEntry(cvi *cnsv1alpha1.CsiVolumeInfo, vmName string) *cnsv1alpha1.VirtualMachineRef {
	for i := range cvi.Spec.VMs {
		if cvi.Spec.VMs[i].VMName == vmName {
			return &cvi.Spec.VMs[i]
		}
	}
	return nil
}

// RemoveVMEntry returns a new slice with the entry for vmName removed.
func RemoveVMEntry(entries []cnsv1alpha1.VirtualMachineRef, vmName string) []cnsv1alpha1.VirtualMachineRef {
	result := entries[:0]
	for _, e := range entries {
		if e.VMName != vmName {
			result = append(result, e)
		}
	}
	return result
}
