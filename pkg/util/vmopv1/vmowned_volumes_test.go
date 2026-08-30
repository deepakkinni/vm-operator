// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package vmopv1_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha6"
	cnsv1alpha1 "github.com/vmware-tanzu/vm-operator/external/vsphere-csi-driver/api/v1alpha1"
	pkgconst "github.com/vmware-tanzu/vm-operator/pkg/constants"
	vmopv1util "github.com/vmware-tanzu/vm-operator/pkg/util/vmopv1"
)

var _ = DescribeTable("HasVMOwnedVolumesAnnotation",
	func(annotations map[string]string, expected bool) {
		vm := &vmopv1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test-vm",
				Namespace:   "test-ns",
				Annotations: annotations,
			},
		}
		Ω(vmopv1util.HasVMOwnedVolumesAnnotation(vm)).Should(Equal(expected))
	},
	Entry("returns true when annotation is present",
		map[string]string{pkgconst.VMOwnedVolumesAnnotation: "true"},
		true,
	),
	Entry("returns true when annotation is present with any value",
		map[string]string{pkgconst.VMOwnedVolumesAnnotation: ""},
		true,
	),
	Entry("returns false when annotation is absent",
		map[string]string{},
		false,
	),
	Entry("returns false when annotations map is nil",
		nil,
		false,
	),
)

var _ = DescribeTable("CVINameForVolumeID",
	func(volumeID, expected string) {
		Ω(vmopv1util.CVINameForVolumeID(volumeID)).Should(Equal(expected))
	},
	Entry("returns prefixed volume ID",
		"abc-123",
		"cvi-volume-abc-123",
	),
	Entry("returns prefix with empty volume ID",
		"",
		"cvi-volume-",
	),
	Entry("returns correct prefix for a UUID-like ID",
		"6b86b273-ff34-febe-9af9-d89cde3b1234",
		"cvi-volume-6b86b273-ff34-febe-9af9-d89cde3b1234",
	),
)

var _ = DescribeTable("IsGreenSignal",
	func(ownership cnsv1alpha1.OwnershipState, phase cnsv1alpha1.PhaseState, generation int64, observedGeneration int64, expected bool) {
		cvi := &cnsv1alpha1.CsiVolumeInfo{
			ObjectMeta: metav1.ObjectMeta{
				Generation: generation,
			},
			Status: cnsv1alpha1.CsiVolumeInfoStatus{
				Ownership:          ownership,
				Phase:              phase,
				ObservedGeneration: observedGeneration,
			},
		}
		Ω(vmopv1util.IsGreenSignal(cvi)).Should(Equal(expected))
	},
	Entry("returns true when all green conditions are met",
		cnsv1alpha1.OwnershipStateVMManaged,
		cnsv1alpha1.PhaseSucceeded,
		int64(1),
		int64(1),
		true,
	),
	Entry("returns true when observedGeneration exceeds generation",
		cnsv1alpha1.OwnershipStateVMManaged,
		cnsv1alpha1.PhaseSucceeded,
		int64(1),
		int64(2),
		true,
	),
	Entry("returns false when ownership is CSIManaged",
		cnsv1alpha1.OwnershipStateCSIManaged,
		cnsv1alpha1.PhaseSucceeded,
		int64(1),
		int64(1),
		false,
	),
	Entry("returns false when ownership is empty",
		cnsv1alpha1.OwnershipState(""),
		cnsv1alpha1.PhaseSucceeded,
		int64(1),
		int64(1),
		false,
	),
	Entry("returns false when phase is Pending",
		cnsv1alpha1.OwnershipStateVMManaged,
		cnsv1alpha1.PhasePending,
		int64(1),
		int64(1),
		false,
	),
	Entry("returns false when phase is Failed",
		cnsv1alpha1.OwnershipStateVMManaged,
		cnsv1alpha1.PhaseFailed,
		int64(1),
		int64(1),
		false,
	),
	Entry("returns false when observedGeneration is less than generation",
		cnsv1alpha1.OwnershipStateVMManaged,
		cnsv1alpha1.PhaseSucceeded,
		int64(3),
		int64(2),
		false,
	),
)

var _ = DescribeTable("VMEntry",
	func(vms []cnsv1alpha1.VirtualMachineRef, vmName string, expected bool) {
		cvi := &cnsv1alpha1.CsiVolumeInfo{
			Spec: cnsv1alpha1.CsiVolumeInfoSpec{
				VMs: vms,
			},
		}
		Ω(vmopv1util.VMEntry(cvi, vmName) != nil).Should(Equal(expected))
	},
	Entry("returns true when spec.vms contains a matching entry",
		[]cnsv1alpha1.VirtualMachineRef{
			{VMName: "my-vm"},
		},
		"my-vm",
		true,
	),
	Entry("returns true when spec.vms contains matching entry among multiple",
		[]cnsv1alpha1.VirtualMachineRef{
			{VMName: "other-vm"},
			{VMName: "my-vm"},
		},
		"my-vm",
		true,
	),
	Entry("returns false when spec.vms is empty",
		[]cnsv1alpha1.VirtualMachineRef{},
		"my-vm",
		false,
	),
	Entry("returns false when spec.vms is nil",
		nil,
		"my-vm",
		false,
	),
	Entry("returns false when no entry matches",
		[]cnsv1alpha1.VirtualMachineRef{
			{VMName: "other-vm"},
			{VMName: "another-vm"},
		},
		"my-vm",
		false,
	),
)

var _ = DescribeTable("DiskModeForVolume / IsDependentMode",
	func(diskMode vmopv1.VolumeDiskMode, expected bool) {
		vol := vmopv1.VirtualMachineVolume{
			DiskMode: diskMode,
		}
		Ω(vmopv1util.IsDependentMode(vmopv1util.DiskModeForVolume(vol))).Should(Equal(expected))
	},
	Entry("returns true for empty diskMode (default persistent)",
		vmopv1.VolumeDiskMode(""),
		true,
	),
	Entry("returns true for VolumeDiskModePersistent",
		vmopv1.VolumeDiskModePersistent,
		true,
	),
	Entry("returns false for VolumeDiskModeIndependentPersistent",
		vmopv1.VolumeDiskModeIndependentPersistent,
		false,
	),
	Entry("returns false for VolumeDiskModeNonPersistent",
		vmopv1.VolumeDiskModeNonPersistent,
		false,
	),
	Entry("returns false for VolumeDiskModeIndependentNonPersistent",
		vmopv1.VolumeDiskModeIndependentNonPersistent,
		false,
	),
)

var _ = DescribeTable("IsMachineOwnedPVC",
	func(owners []metav1.OwnerReference, expected bool) {
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "my-pvc",
				Namespace:       "test-ns",
				OwnerReferences: owners,
			},
		}
		Ω(vmopv1util.IsMachineOwnedPVC(pvc)).Should(Equal(expected))
	},
	Entry("returns false when there are no owner references",
		nil,
		false,
	),
	Entry("returns true when owned by a VirtualMachine",
		[]metav1.OwnerReference{{Kind: "VirtualMachine", Name: "my-vm"}},
		true,
	),
	Entry("returns true when owned by a VSphereMachine",
		[]metav1.OwnerReference{{Kind: "VSphereMachine", Name: "my-machine"}},
		true,
	),
	Entry("returns false when owned by an unrelated kind",
		[]metav1.OwnerReference{{Kind: "Deployment", Name: "my-deployment"}},
		false,
	),
	Entry("returns true when one of multiple owners matches",
		[]metav1.OwnerReference{
			{Kind: "Deployment", Name: "my-deployment"},
			{Kind: "VirtualMachine", Name: "my-vm"},
		},
		true,
	),
)

var _ = Describe("GetCVIForPVC", func() {
	const (
		ns       = "test-ns"
		pvcName  = "my-pvc"
		pvName   = "my-pv"
		volumeID = "abc-volume-id-123"
	)

	var (
		scheme *runtime.Scheme
	)

	BeforeEach(func() {
		scheme = runtime.NewScheme()
		Ω(clientgoscheme.AddToScheme(scheme)).To(Succeed())
		Ω(cnsv1alpha1.AddToScheme(scheme)).To(Succeed())
	})

	buildPVC := func(volumeName string) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pvcName,
				Namespace: ns,
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				VolumeName: volumeName,
			},
		}
	}

	buildPV := func(name string, csiSource *corev1.CSIPersistentVolumeSource) *corev1.PersistentVolume {
		pv := &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
			},
			Spec: corev1.PersistentVolumeSpec{},
		}
		if csiSource != nil {
			pv.Spec.CSI = csiSource
		}
		return pv
	}

	buildCVI := func(volumeID string) *cnsv1alpha1.CsiVolumeInfo {
		return &cnsv1alpha1.CsiVolumeInfo{
			ObjectMeta: metav1.ObjectMeta{
				Name:      vmopv1util.CVINameForVolumeID(volumeID),
				Namespace: cnsv1alpha1.CVINamespace,
			},
		}
	}

	It("returns the CVI when PVC is bound to a PV with a CSI source", func() {
		pvc := buildPVC(pvName)
		pv := buildPV(pvName, &corev1.CSIPersistentVolumeSource{
			VolumeHandle: volumeID,
		})
		cvi := buildCVI(volumeID)

		c := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(pvc, pv, cvi).
			Build()

		result, err := vmopv1util.GetCVIForPVC(context.Background(), c, ns, pvcName)
		Ω(err).ShouldNot(HaveOccurred())
		Ω(result).ShouldNot(BeNil())
		Ω(result.Name).Should(Equal(vmopv1util.CVINameForVolumeID(volumeID)))
	})

	It("returns error when PVC does not exist", func() {
		c := fake.NewClientBuilder().
			WithScheme(scheme).
			Build()

		result, err := vmopv1util.GetCVIForPVC(context.Background(), c, ns, pvcName)
		Ω(err).Should(HaveOccurred())
		Ω(err.Error()).Should(ContainSubstring("failed to get PVC"))
		Ω(result).Should(BeNil())
	})

	It("returns error when PVC is unbound (spec.volumeName is empty)", func() {
		pvc := buildPVC("") // empty volumeName = unbound

		c := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(pvc).
			Build()

		result, err := vmopv1util.GetCVIForPVC(context.Background(), c, ns, pvcName)
		Ω(err).Should(HaveOccurred())
		Ω(err.Error()).Should(ContainSubstring("not yet bound"))
		Ω(result).Should(BeNil())
	})

	It("returns error when PV has no CSI source", func() {
		pvc := buildPVC(pvName)
		pv := buildPV(pvName, nil) // nil CSI source

		c := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(pvc, pv).
			Build()

		result, err := vmopv1util.GetCVIForPVC(context.Background(), c, ns, pvcName)
		Ω(err).Should(HaveOccurred())
		Ω(err.Error()).Should(ContainSubstring("does not have a CSI source"))
		Ω(result).Should(BeNil())
	})

	It("returns not-found-compatible error when CVI does not exist", func() {
		pvc := buildPVC(pvName)
		pv := buildPV(pvName, &corev1.CSIPersistentVolumeSource{
			VolumeHandle: volumeID,
		})
		// No CVI object created

		c := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(pvc, pv).
			Build()

		result, err := vmopv1util.GetCVIForPVC(context.Background(), c, ns, pvcName)
		Ω(err).Should(HaveOccurred())
		Ω(err.Error()).Should(ContainSubstring("failed to get CsiVolumeInfo"))
		Ω(result).Should(BeNil())
	})
})
