// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

// Package owned holds the pure classification logic for VM-owned-volumes
// disks, kept out of controllers/ per the repository's thin-controller rule.
package owned

import (
	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha6"
	cnsv1alpha1 "github.com/vmware-tanzu/vm-operator/external/vsphere-csi-driver/api/v1alpha1"
	vmopv1util "github.com/vmware-tanzu/vm-operator/pkg/util/vmopv1"
)

// VolumePlan is the per-volume disposition of a PVC-backed disk on a
// VM-owned VM: which CsiVolumeInfo it maps to and which of the two
// ownership behaviors (dependent transfer vs. independent registered-FCD)
// applies.
type VolumePlan struct {
	// VolumeName is vm.spec.volumes[*].name.
	VolumeName string
	// ClaimName is the PVC claim name backing the volume.
	ClaimName string
	// DiskMode is the CsiVolumeInfo-side disk mode for this volume.
	DiskMode cnsv1alpha1.CVIDiskMode
	// RawDiskMode is vm.spec.volumes[*].diskMode verbatim (may be empty),
	// for callers building a vSphere device spec, which speaks
	// vmopv1.VolumeDiskMode rather than the CVI's mirrored enum.
	RawDiskMode vmopv1.VolumeDiskMode
	// Dependent is true when DiskMode requires CSI's ownership-transfer
	// behavior (best-effort unregister). False means the FCD stays
	// registered and CSIManaged.
	Dependent bool
	// SharingMode is vm.spec.volumes[*].sharingMode.
	SharingMode vmopv1.VolumeSharingMode
	// ControllerType, ControllerBusNumber, and UnitNumber are the user's
	// explicit slot request, if any.
	ControllerType      vmopv1.VirtualControllerType
	ControllerBusNumber *int32
	UnitNumber          *int32
}

// ClassifyVolumes returns the plan for every PVC-backed volume in
// vm.spec.volumes. On a VM-owned VM every disk mode is coordinated through
// its CsiVolumeInfo (attach/detach §2.7); callers are expected to invoke
// this only once the VM-owned-volumes feature gate and per-VM annotation
// have both been confirmed.
func ClassifyVolumes(vm *vmopv1.VirtualMachine) []VolumePlan {
	plans := make([]VolumePlan, 0, len(vm.Spec.Volumes))

	for _, vol := range vm.Spec.Volumes {
		if vol.PersistentVolumeClaim == nil {
			continue
		}

		diskMode := vmopv1util.DiskModeForVolume(vol)
		plans = append(plans, VolumePlan{
			VolumeName:          vol.Name,
			ClaimName:           vol.PersistentVolumeClaim.ClaimName,
			DiskMode:            diskMode,
			RawDiskMode:         vol.DiskMode,
			Dependent:           vmopv1util.IsDependentMode(diskMode),
			SharingMode:         vol.SharingMode,
			ControllerType:      vol.ControllerType,
			ControllerBusNumber: vol.ControllerBusNumber,
			UnitNumber:          vol.UnitNumber,
		})
	}

	return plans
}
