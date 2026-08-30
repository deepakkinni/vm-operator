// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha6"
)

// VolumeDiskAddSpec describes one VM-owned-volumes disk to add to a VM in a
// batched AttachVolumeDisks call.
type VolumeDiskAddSpec struct {
	// VolumeName is vm.spec.volumes[*].name.
	VolumeName string
	// DiskPath is the VMDK datastore path to attach.
	DiskPath string
	// DiskMode is the mode to attach the disk in.
	DiskMode vmopv1.VolumeDiskMode
	// SharingMode is the disk's sharing mode. Only MultiWriter changes the
	// device built; None is the vSphere default.
	SharingMode vmopv1.VolumeSharingMode
	// ControllerType, ControllerBusNumber, and UnitNumber pin the device to
	// an explicit slot when set. When any is unset the provider picks the
	// first SCSI controller with a free slot.
	ControllerType      vmopv1.VirtualControllerType
	ControllerBusNumber *int32
	UnitNumber          *int32
	// FcdID is set iff the disk is a still-registered, linked-clone FCD
	// (dependent fcd-retained today; independent starting in V4). Left
	// unset for a still-FCD disk that is not a linked clone, so its
	// device-add does not carry vDiskId and does not route through
	// vpxd's FCD-identity reconfigure prechecks.
	FcdID string
	// CBTEnabled is set iff FcdID is set and the mode is independent.
	// Consumed starting in V4.
	CBTEnabled *bool
}

// VolumeDiskPlacement is the resolved device slot for a disk added (or
// already present) via AttachVolumeDisks.
type VolumeDiskPlacement struct {
	// VolumeName echoes the corresponding VolumeDiskAddSpec.VolumeName.
	VolumeName string
	// DiskUUID is the attached VirtualDisk's backing UUID.
	DiskUUID string
	// ControllerType, ControllerBusNumber, and UnitNumber are the device's
	// resolved slot.
	ControllerType      vmopv1.VirtualControllerType
	ControllerBusNumber int32
	UnitNumber          int32
}

// VolumeDiskModeSlot identifies one already-attached disk by its observed
// device slot, for a batched ConvertDisksToIndependentPersistent call.
type VolumeDiskModeSlot struct {
	// VolumeName is vm.spec.volumes[*].name.
	VolumeName string
	// ControllerType, ControllerBusNumber, and UnitNumber pin the disk's
	// current device slot, as recorded in vm.status.volumes.
	ControllerType      vmopv1.VirtualControllerType
	ControllerBusNumber int32
	UnitNumber          int32
}
