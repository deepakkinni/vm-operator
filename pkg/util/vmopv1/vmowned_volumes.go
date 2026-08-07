// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package vmopv1

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha6"
	cnsv1alpha1 "github.com/vmware-tanzu/vm-operator/external/vsphere-csi-driver/api/v1alpha1"
	pkgconst "github.com/vmware-tanzu/vm-operator/pkg/constants"
)

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

// GetCVIForPVC resolves the CsiVolumeInfo for the given PVC by looking up the
// PV from the PVC's spec.volumeName and extracting the volumeHandle. Returns
// an apierrors.IsNotFound-compatible error if the PVC, PV, or CVI is not found.
func GetCVIForPVC(
	ctx context.Context,
	c ctrlclient.Client,
	pvcNamespace, pvcName string) (*cnsv1alpha1.CsiVolumeInfo, error) {

	// 1. Get the PVC.
	pvc := &corev1.PersistentVolumeClaim{}
	if err := c.Get(ctx, ctrlclient.ObjectKey{
		Namespace: pvcNamespace,
		Name:      pvcName,
	}, pvc); err != nil {
		return nil, fmt.Errorf("failed to get PVC %s/%s: %w", pvcNamespace, pvcName, err)
	}

	// 2. Get the PV from the PVC's spec.volumeName.
	pvName := pvc.Spec.VolumeName
	if pvName == "" {
		return nil, fmt.Errorf("PVC %s/%s is not yet bound to a PV", pvcNamespace, pvcName)
	}

	pv := &corev1.PersistentVolume{}
	if err := c.Get(ctx, ctrlclient.ObjectKey{Name: pvName}, pv); err != nil {
		return nil, fmt.Errorf("failed to get PV %s for PVC %s/%s: %w", pvName, pvcNamespace, pvcName, err)
	}

	// 3. Extract volumeID from the PV's CSI source.
	if pv.Spec.CSI == nil {
		return nil, fmt.Errorf("PV %s does not have a CSI source", pvName)
	}
	volumeID := pv.Spec.CSI.VolumeHandle
	if volumeID == "" {
		return nil, fmt.Errorf("PV %s has an empty CSI volumeHandle", pvName)
	}

	// 4. Get CVI by name in the CSI system namespace.
	cvi := &cnsv1alpha1.CsiVolumeInfo{}
	if err := c.Get(ctx, ctrlclient.ObjectKey{
		Namespace: cnsv1alpha1.CVINamespace,
		Name:      CVINameForVolumeID(volumeID),
	}, cvi); err != nil {
		return nil, fmt.Errorf("failed to get CsiVolumeInfo for volume %s: %w", volumeID, err)
	}

	return cvi, nil
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
