// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// VolumeProtectionFinalizer prevents GC while ownership is VMManaged.
	VolumeProtectionFinalizer = "csi.vsphere.vmware.com/volume-protection"

	// PVCVolumeProtectionFinalizer is written by CSI's CsiVolumeInfo controller
	// onto the bound PVC while spec.vms is non-empty. It is the only thing
	// preventing deletion of an attached PVC for an independent volume, since
	// that CsiVolumeInfo never transitions to VMManaged and
	// VolumeProtectionFinalizer above never applies to it. vm-operator does not
	// write this finalizer and must not be surprised by its presence.
	PVCVolumeProtectionFinalizer = "csi.vsphere.vmware.com/pvc-volume-protection"

	// CVINamespace is the namespace where CsiVolumeInfo CRs live.
	CVINamespace = "vmware-system-csi"

	// CVINamePrefix is the prefix used to construct a CsiVolumeInfo CR name
	// from the CNS volume ID. The full name is CVINamePrefix + volumeID.
	CVINamePrefix = "cvi-volume-"

	// FcdRetainedAnnotation marks a VMManaged volume whose FCD was NOT
	// unregistered because an in-place unregister was blocked. The FCD, its
	// CNS DB row, and its FCD snapshots all still exist, so lock-down for such
	// a volume must be enforced by consulting this annotation rather than by
	// relying on CNS to return NotFound.
	FcdRetainedAnnotation = "csi.vsphere.vmware.com/fcd-retained"
)

// OwnershipState is the current ownership of the volume.
type OwnershipState string

const (
	// OwnershipStateCSIManaged is the steady state when the volume is a
	// registered FCD managed by CSI.
	OwnershipStateCSIManaged OwnershipState = "CSIManaged"

	// OwnershipStateVMManaged is the steady state when the disk is a plain
	// VMDK managed by a greenfield VM.
	OwnershipStateVMManaged OwnershipState = "VMManaged"
)

// PhaseState represents the reconcile phase of a CsiVolumeInfo.
type PhaseState string

const (
	// PhasePending indicates the controller has not yet acted on the current
	// spec generation.
	PhasePending PhaseState = "Pending"

	// PhaseSucceeded indicates the last reconcile completed successfully.
	PhaseSucceeded PhaseState = "Succeeded"

	// PhaseFailed indicates the last reconcile encountered an error.
	PhaseFailed PhaseState = "Failed"
)

// CVIDiskMode is the disk mode a VM attaches a volume in, mirroring
// vmopv1.VolumeDiskMode. Named distinctly from this package's existing
// DiskMode (CnsNodeVMBatchAttachment's lower-snake-case enum) because both
// types live in this single mirrored package; CSI keeps them apart in
// separate Go packages. The JSON wire values match CSI's DiskMode exactly.
type CVIDiskMode string

const (
	// CVIDiskModePersistent is the dependent mode: CSI transfers ownership
	// of the FCD to the VM via a best-effort unregister.
	CVIDiskModePersistent CVIDiskMode = "Persistent"
	// CVIDiskModeIndependentPersistent is an independent mode: the FCD stays
	// registered and CSIManaged.
	CVIDiskModeIndependentPersistent CVIDiskMode = "IndependentPersistent"
	// CVIDiskModeIndependentNonPersistent is an independent mode: the FCD
	// stays registered and CSIManaged.
	CVIDiskModeIndependentNonPersistent CVIDiskMode = "IndependentNonPersistent"
	// CVIDiskModeNonPersistent is treated like an independent mode for
	// ownership purposes: the FCD stays registered and CSIManaged.
	CVIDiskModeNonPersistent CVIDiskMode = "NonPersistent"
)

// VirtualMachineRef identifies a VM attached to the volume.
type VirtualMachineRef struct {
	// VMName is the VirtualMachine CR name.
	VMName string `json:"vmName"`

	// VMInstanceUUID is the instance UUID of the VM.
	// +optional
	VMInstanceUUID string `json:"vmInstanceUUID,omitempty"`

	// DiskMode is the disk mode this VM attaches the volume in. CSI keys the
	// ownership-transfer decision on it: a Persistent (dependent) entry
	// triggers the best-effort unregister, while an independent entry leaves
	// the FCD registered and the volume CSIManaged. Written by vm-operator,
	// mirroring vm.spec.volumes[*].diskMode. An empty value is treated as
	// Persistent, matching the vm.spec default.
	// +optional
	DiskMode CVIDiskMode `json:"diskMode,omitempty"`

	// VolumeName is vm.spec.volumes[*].name on that VM. vm-operator writes it
	// so that a detach can correlate this entry to its vm.status.volumes
	// entry — and therefore to the device slot — after the volume has
	// already been removed from vm.spec.volumes. CSI does not read it.
	// +optional
	VolumeName string `json:"volumeName,omitempty"`
}

// CsiVolumeInfoSpec defines the desired state of CsiVolumeInfo.
type CsiVolumeInfoSpec struct {
	// VolumeID is the CNS volume ID. This field is immutable after creation.
	// +required
	VolumeID string `json:"volumeID"`

	// PVCName is the name of the bound PersistentVolumeClaim.
	// +optional
	PVCName string `json:"pvcName,omitempty"`

	// PVCNamespace is the namespace of the bound PersistentVolumeClaim. The
	// CsiVolumeInfo CR itself lives in CVINamespace.
	// +optional
	PVCNamespace string `json:"pvcNamespace,omitempty"`

	// PVName is the name of the bound PersistentVolume.
	// +optional
	PVName string `json:"pvName,omitempty"`

	// DiskUUID is the disk UUID for the CNS volume. This field is
	// informational and may drift from the authoritative value. Unset for
	// an fcd-retained volume, since the capture that fills it never runs on
	// that path.
	// +optional
	DiskUUID string `json:"diskUUID,omitempty"`

	// DiskPath is the VMDK datastore path for the CNS volume. CSI writes
	// this field at Unregister time; vm-operator refreshes it just-in-time
	// before attaching.
	// +optional
	DiskPath string `json:"diskPath,omitempty"`

	// VMs lists the VirtualMachine objects that have a relationship with
	// this volume. vm-operator is the sole writer of this field. The
	// presence of an entry indicates an attached or snapshot-retained
	// relationship.
	// +optional
	// +listType=map
	// +listMapKey=vmName
	VMs []VirtualMachineRef `json:"vms,omitempty"`
}

// CsiVolumeInfoStatus defines the observed state of CsiVolumeInfo.
type CsiVolumeInfoStatus struct {
	// Ownership indicates who currently manages this volume.
	// +optional
	Ownership OwnershipState `json:"ownership,omitempty"`

	// Phase is the current lifecycle phase of the volume.
	// +optional
	Phase PhaseState `json:"phase,omitempty"`

	// ObservedGeneration is the generation of the spec that was last
	// processed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Error contains a human-readable error message when Phase is Failed.
	// +optional
	Error string `json:"error,omitempty"`

	// Conditions describes the current conditions of the CsiVolumeInfo.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// +k8s:openapi-gen=true
// +kubebuilder:subresource:status
// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=cvi,path=csivolumeinfos,singular=csivolumeinfo

// CsiVolumeInfo is the Schema for the csivolumeinfolist API. It tracks
// per-volume ownership between the CSI driver and vm-operator. vm-operator
// writes spec.vms; CSI writes status.
type CsiVolumeInfo struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CsiVolumeInfoSpec   `json:"spec,omitempty"`
	Status CsiVolumeInfoStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// CsiVolumeInfoList contains a list of CsiVolumeInfo.
type CsiVolumeInfoList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CsiVolumeInfo `json:"items"`
}

func init() {
	objectTypes = append(objectTypes, &CsiVolumeInfo{}, &CsiVolumeInfoList{})
}
