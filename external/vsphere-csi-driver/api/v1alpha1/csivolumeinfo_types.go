// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// CsiVolumeInfoFinalizer is the finalizer placed on CsiVolumeInfo objects
	// by the CSI driver to protect them from premature deletion.
	CsiVolumeInfoFinalizer = "csi.vsphere.vmware.com/volume-protection"

	// CsiVolumeInfoNamePrefix is the prefix used to construct a CsiVolumeInfo
	// CR name from the CNS volume ID. The full name is CsiVolumeInfoNamePrefix
	// + volumeID.
	CsiVolumeInfoNamePrefix = "cns-volume-"

	// CsiVolumeInfoNamespace is the namespace where CsiVolumeInfo CRs live.
	CsiVolumeInfoNamespace = "vmware-system-csi"
)

// CsiVolumeOwnership describes who currently manages a CNS volume.
type CsiVolumeOwnership string

const (
	// OwnershipCSIManaged indicates the volume is managed by the CSI driver.
	OwnershipCSIManaged CsiVolumeOwnership = "CSIManaged"

	// OwnershipVMManaged indicates the volume is managed by vm-operator.
	OwnershipVMManaged CsiVolumeOwnership = "VMManaged"
)

// CsiVolumePhase describes the current lifecycle phase of a CsiVolumeInfo.
type CsiVolumePhase string

const (
	// PhasePending indicates the volume operation is in progress.
	PhasePending CsiVolumePhase = "Pending"

	// PhaseSucceeded indicates the volume operation completed successfully.
	PhaseSucceeded CsiVolumePhase = "Succeeded"

	// PhaseFailed indicates the volume operation failed.
	PhaseFailed CsiVolumePhase = "Failed"
)

// CsiVolumeInfoVMEntry records the relationship between a CNS volume and a
// VirtualMachine. Its presence in spec.vms indicates an attach or
// snapshot-retained relationship.
type CsiVolumeInfoVMEntry struct {
	// VMName is the name of the VirtualMachine object.
	VMName string `json:"vmName"`

	// VMInstanceUUID is the instance UUID of the vSphere VM.
	VMInstanceUUID string `json:"vmInstanceUUID"`
}

// CsiVolumeInfoSpec defines the desired state of CsiVolumeInfo.
type CsiVolumeInfoSpec struct {
	// VolumeID is the CNS volume ID. This field is immutable after creation.
	// +required
	VolumeID string `json:"volumeID"`

	// PVC is the name of the bound PersistentVolumeClaim.
	// +optional
	PVC string `json:"pvc,omitempty"`

	// PVCNamespace is the namespace of the bound PersistentVolumeClaim. The
	// CsiVolumeInfo CR itself lives in CsiVolumeInfoNamespace.
	// +optional
	PVCNamespace string `json:"pvcNamespace,omitempty"`

	// PVName is the name of the bound PersistentVolume.
	// +optional
	PVName string `json:"pvName,omitempty"`

	// DiskUUID is the disk UUID for the CNS volume. This field is
	// informational and may drift from the authoritative value.
	// +optional
	DiskUUID string `json:"diskUUID,omitempty"`

	// DiskPath is the VMDK datastore path for the CNS volume. CSI writes this
	// field at Unregister time; vm-operator refreshes it just-in-time before
	// attaching.
	// +optional
	DiskPath string `json:"diskPath,omitempty"`

	// VMs lists the VirtualMachine objects that have a relationship with this
	// volume. vm-operator is the sole writer of this field. The presence of an
	// entry indicates an attached or snapshot-retained relationship.
	// +optional
	// +listType=map
	// +listMapKey=vmName
	VMs []CsiVolumeInfoVMEntry `json:"vms,omitempty"`
}

// CsiVolumeInfoStatus defines the observed state of CsiVolumeInfo.
type CsiVolumeInfoStatus struct {
	// Ownership indicates who currently manages this volume.
	// +optional
	Ownership CsiVolumeOwnership `json:"ownership,omitempty"`

	// Phase is the current lifecycle phase of the volume.
	// +optional
	Phase CsiVolumePhase `json:"phase,omitempty"`

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
// +kubebuilder:resource:shortName=cvi

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
