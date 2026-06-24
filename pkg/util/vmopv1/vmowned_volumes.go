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
	return pkgconst.CVINamePrefix + volumeID
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
		Namespace: pkgconst.CVISystemNamespace,
		Name:      CVINameForVolumeID(volumeID),
	}, cvi); err != nil {
		return nil, fmt.Errorf("failed to get CsiVolumeInfo for volume %s: %w", volumeID, err)
	}

	return cvi, nil
}

// IsGreenSignal reports whether the CsiVolumeInfo status has the green signal
// that permits vm-operator to add the disk to the VM.
// Green signal = status.ownership==VMManaged && status.phase==Succeeded
// && status.observedGeneration >= metadata.generation.
func IsGreenSignal(cvi *cnsv1alpha1.CsiVolumeInfo) bool {
	return cvi.Status.Ownership == cnsv1alpha1.OwnershipVMManaged &&
		cvi.Status.Phase == cnsv1alpha1.PhaseSucceeded &&
		cvi.Status.ObservedGeneration >= cvi.Generation
}

// HasVMEntry reports whether spec.vms contains an entry for the given vmName.
func HasVMEntry(cvi *cnsv1alpha1.CsiVolumeInfo, vmName string) bool {
	for _, entry := range cvi.Spec.VMs {
		if entry.VMName == vmName {
			return true
		}
	}
	return false
}

// IsDependentPersistentMode reports whether the volume's disk mode requires
// ownership transfer (only Persistent mode does).
func IsDependentPersistentMode(vol vmopv1.VirtualMachineVolume) bool {
	// Only VolumeDiskModePersistent (empty = default = Persistent) triggers the path.
	dm := vol.DiskMode
	return dm == "" || dm == vmopv1.VolumeDiskModePersistent
}
