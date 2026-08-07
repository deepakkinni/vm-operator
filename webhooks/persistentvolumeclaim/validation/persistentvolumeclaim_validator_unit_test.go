// © Broadcom. All Rights Reserved.
// The term “Broadcom” refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package validation_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation/field"

	cnsv1alpha1 "github.com/vmware-tanzu/vm-operator/external/vsphere-csi-driver/api/v1alpha1"
	pkgcfg "github.com/vmware-tanzu/vm-operator/pkg/config"
	"github.com/vmware-tanzu/vm-operator/pkg/constants/testlabels"
	"github.com/vmware-tanzu/vm-operator/pkg/providers/vsphere/constants"
	"github.com/vmware-tanzu/vm-operator/test/builder"
)

const (
	DummyLabelKey1   = "dummy-key-1"
	DummyLabelKey2   = "dummy-key-2"
	DummyLabelValue1 = "dummy-value-1"
	DummyLabelValue2 = "dummy-value-2"
	TrueString       = "true"
)

func unitTests() {
	Describe(
		"Create",
		Label(
			testlabels.Create,
			testlabels.API,
			testlabels.Validation,
			testlabels.Webhook,
		),
		unitTestsValidatePVCCreate,
	)
	Describe(
		"Update",
		Label(
			testlabels.Update,
			testlabels.API,
			testlabels.Validation,
			testlabels.Webhook,
		),
		unitTestsValidatePVCUpdate,
	)
	Describe(
		"Delete",
		Label(
			testlabels.Delete,
			testlabels.API,
			testlabels.Validation,
			testlabels.Webhook,
		),
		unitTestsValidatePVCDelete,
	)
	Describe(
		"Delete of a VM-owned volume",
		Label(
			testlabels.Delete,
			testlabels.API,
			testlabels.Validation,
			testlabels.Webhook,
		),
		unitTestsValidateVMOwnedPVCDelete,
	)
}

type unitValidatingWebhookContext struct {
	builder.UnitTestContextForValidatingWebhook
	pvc    *corev1.PersistentVolumeClaim
	pvcOld *corev1.PersistentVolumeClaim
}

func newUnitTestContextForValidatingWebhook(isUpdate bool) *unitValidatingWebhookContext {
	pvc := builder.DummyPersistentVolumeClaim()
	obj, err := builder.ToUnstructured(pvc)
	Expect(err).ToNot(HaveOccurred())

	var oldPVC *corev1.PersistentVolumeClaim
	var oldObj *unstructured.Unstructured

	if isUpdate {
		oldPVC = pvc.DeepCopy()
		oldObj, err = builder.ToUnstructured(oldPVC)
		Expect(err).ToNot(HaveOccurred())
	}

	return &unitValidatingWebhookContext{
		UnitTestContextForValidatingWebhook: *suite.NewUnitTestContextForValidatingWebhook(obj, oldObj),
		pvc:                                 pvc,
		pvcOld:                              oldPVC,
	}
}

func unitTestsValidatePVCCreate() {
	var (
		ctx *unitValidatingWebhookContext
	)
	type createArgs struct {
		addNonInstanceStorageLabel bool
		addInstanceStorageLabel    bool
		isServiceUser              bool
	}
	validateCreate := func(args createArgs, expectedAllowed bool, expectedReason string, expectedErr error) {
		var err error

		if args.isServiceUser {
			ctx.IsPrivilegedAccount = true
		}

		if args.addInstanceStorageLabel {
			ctx.pvc.Labels = map[string]string{
				constants.InstanceStorageLabelKey: TrueString,
			}
		}

		if args.addNonInstanceStorageLabel {
			ctx.pvc.Labels = map[string]string{
				DummyLabelKey1: DummyLabelValue1,
				DummyLabelKey2: DummyLabelValue2,
			}
		}

		ctx.WebhookRequestContext.Obj, err = builder.ToUnstructured(ctx.pvc)
		Expect(err).ToNot(HaveOccurred())

		response := ctx.ValidateCreate(&ctx.WebhookRequestContext)
		Expect(response.Allowed).To(Equal(expectedAllowed))
		if expectedReason != "" {
			Expect(string(response.Result.Reason)).To(ContainSubstring(expectedReason))
		}
		if expectedErr != nil {
			Expect(response.Result.Message).To(Equal(expectedErr.Error()))
		}
	}

	BeforeEach(func() {
		ctx = newUnitTestContextForValidatingWebhook(false)
	})
	AfterEach(func() {
		ctx = nil
	})
	labelPath := field.NewPath("metadata", "labels").Key(constants.InstanceStorageLabelKey)
	DescribeTable("create table", validateCreate,
		Entry("SSO user, should allow with empty labels", createArgs{}, true, nil, nil),
		Entry("SSO user, should allow with labels not specific to instance storage", createArgs{addNonInstanceStorageLabel: true}, true, nil, nil),
		Entry("SSO user, should deny with labels specific to instance storage", createArgs{addInstanceStorageLabel: true}, false,
			field.Forbidden(labelPath, "CREATE operation on PVC with instance storage label is not allowed").Error(), nil),
		Entry("Service user, should allow with labels specific to instance storage", createArgs{isServiceUser: true, addInstanceStorageLabel: true}, true, nil, nil),
		Entry("Service user, should allow with labels not specific to instance storage", createArgs{isServiceUser: true, addNonInstanceStorageLabel: true}, true, nil, nil),
	)
}

func unitTestsValidatePVCUpdate() {
	var (
		ctx *unitValidatingWebhookContext
	)
	type updateArgs struct {
		addNonInstanceStorageLabel         bool
		addInstanceStorageLabel            bool
		updateNonInstanceStorageLabel      bool
		updateInstanceStorageLabel         bool
		removeNonInstanceStorageLabel      bool
		addInstanceStorageLabelToOldPVC    bool
		addNonInstanceStorageLabelToOldPVC bool
		isServiceUser                      bool
	}

	validateUpdate := func(args updateArgs, expectedAllowed bool, expectedReason string, expectedErr error) {
		var err error
		if args.isServiceUser {
			ctx.IsPrivilegedAccount = true
		}
		if args.addNonInstanceStorageLabel {
			ctx.pvc.Labels[DummyLabelKey1] = DummyLabelValue1
		}
		if args.addInstanceStorageLabel {
			ctx.pvc.Labels[constants.InstanceStorageLabelKey] = TrueString
		}
		if args.addNonInstanceStorageLabelToOldPVC {
			ctx.pvcOld.Labels[DummyLabelKey1] = DummyLabelKey1
		}
		if args.updateNonInstanceStorageLabel {
			ctx.pvc.Labels[DummyLabelKey1] = DummyLabelValue1 + "-updated"
		}
		if args.addInstanceStorageLabelToOldPVC {
			ctx.pvcOld.Labels[constants.InstanceStorageLabelKey] = TrueString
		}
		if args.updateInstanceStorageLabel {
			ctx.pvc.Labels[constants.InstanceStorageLabelKey] = TrueString
		}
		if args.removeNonInstanceStorageLabel {
			ctx.pvcOld.Labels[DummyLabelKey1] = DummyLabelValue1 + "-updated"
		}

		ctx.WebhookRequestContext.Obj, err = builder.ToUnstructured(ctx.pvc)
		Expect(err).ToNot(HaveOccurred())

		ctx.WebhookRequestContext.OldObj, err = builder.ToUnstructured(ctx.pvcOld)
		Expect(err).ToNot(HaveOccurred())

		response := ctx.ValidateUpdate(&ctx.WebhookRequestContext)
		Expect(response.Allowed).To(Equal(expectedAllowed))
		if expectedReason != "" {
			Expect(string(response.Result.Reason)).To(ContainSubstring(expectedReason))
		}
		if expectedErr != nil {
			Expect(response.Result.Message).To(Equal(expectedErr.Error()))
		}
	}
	BeforeEach(func() {
		ctx = newUnitTestContextForValidatingWebhook(true)
	})
	AfterEach(func() {
		ctx = nil
	})
	labelPath := field.NewPath("metadata", "labels").Key(constants.InstanceStorageLabelKey)
	DescribeTable("create table", validateUpdate,
		Entry("SSO user, should allow adding non instance storage labels", updateArgs{addNonInstanceStorageLabel: true}, true, nil, nil),
		Entry("SSO user, should allow updating non instance storage labels", updateArgs{addNonInstanceStorageLabelToOldPVC: true, updateNonInstanceStorageLabel: true}, true, nil, nil),
		Entry("SSO user, should allow to remove labels non specific to instance storage", updateArgs{removeNonInstanceStorageLabel: true}, true, nil, nil),
		Entry("SSO user, should deny adding labels specific to instance storage", updateArgs{addInstanceStorageLabel: true}, false,
			field.Forbidden(labelPath, "adding instance storage label is not allowed").Error(), nil),
		Entry("SSO user, should deny updating labels specific to instance storage", updateArgs{addInstanceStorageLabelToOldPVC: true, updateInstanceStorageLabel: true}, false,
			field.Forbidden(labelPath, "UPDATE operation on PVC with instance storage label is not allowed").Error(), nil),
		Entry("SSO user, should deny to remove labels specific to instance storage", updateArgs{addInstanceStorageLabelToOldPVC: true}, false,
			field.Forbidden(labelPath, "UPDATE operation on PVC with instance storage label is not allowed").Error(), nil),
		Entry("Service user, should allow adding labels specific to instance storage", updateArgs{isServiceUser: true, addInstanceStorageLabel: true}, true, nil, nil),
		Entry("Service user, should allow to update labels specific to instance storage", updateArgs{isServiceUser: true, addInstanceStorageLabelToOldPVC: true, updateInstanceStorageLabel: true}, true, nil, nil),
		Entry("Service user, should allow to remove labels specific to instance storage", updateArgs{isServiceUser: true, addInstanceStorageLabelToOldPVC: true}, true, nil, nil),
	)
}

func unitTestsValidatePVCDelete() {
	var (
		ctx *unitValidatingWebhookContext
	)
	type deleteArgs struct {
		addInstanceStorageLabels bool
		isServiceUser            bool
	}
	validateDelete := func(args deleteArgs, expectedAllowed bool, expectedReason string, expectedErr error) {
		var err error
		if args.addInstanceStorageLabels {
			ctx.pvc.Labels[constants.InstanceStorageLabelKey] = TrueString
		}
		if args.isServiceUser {
			ctx.IsPrivilegedAccount = true
		}

		ctx.WebhookRequestContext.Obj, err = builder.ToUnstructured(ctx.pvc)
		Expect(err).ToNot(HaveOccurred())
		response := ctx.ValidateDelete(&ctx.WebhookRequestContext)
		Expect(response.Allowed).To(Equal(expectedAllowed))
		if expectedReason != "" {
			Expect(string(response.Result.Reason)).To(ContainSubstring(expectedReason))
		}
		if expectedErr != nil {
			Expect(response.Result.Message).To(Equal(expectedErr.Error()))
		}
	}
	BeforeEach(func() {
		ctx = newUnitTestContextForValidatingWebhook(false)
	})
	AfterEach(func() {
		ctx = nil
	})
	labelPath := field.NewPath("metadata", "labels").Key(constants.InstanceStorageLabelKey)
	DescribeTable("create table", validateDelete,
		Entry("SSO user, should allow delete if instance storage labels are not present", deleteArgs{}, true, nil, nil),
		Entry("SSO user, should deny delete if instance storage labels are present", deleteArgs{addInstanceStorageLabels: true}, false,
			field.Forbidden(labelPath, "DELETE operation on PVC with instance storage label is not allowed").Error(), nil),
		Entry("Service user, should allow delete if instance storage labels are present", deleteArgs{isServiceUser: true, addInstanceStorageLabels: true}, true, nil, nil),
	)
}

func unitTestsValidateVMOwnedPVCDelete() {
	const (
		pvName    = "validate-webhook-pv"
		volumeID  = "vol-owned-1"
		vmName    = "vm-owned-1"
		namespace = "default"
	)

	var (
		ctx *unitValidatingWebhookContext
		pv  *corev1.PersistentVolume
		cvi *cnsv1alpha1.CsiVolumeInfo
	)

	BeforeEach(func() {
		ctx = newUnitTestContextForValidatingWebhook(false)
		pkgcfg.UpdateContext(ctx, func(config *pkgcfg.Config) {
			config.Features.VMOwnedVolumes = true
		})
		ctx.pvc.Namespace = namespace
		ctx.pvc.Spec.VolumeName = pvName

		pv = &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{
				Name: pvName,
			},
			Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{
						VolumeHandle: volumeID,
					},
				},
			},
		}

		cvi = &cnsv1alpha1.CsiVolumeInfo{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cnsv1alpha1.CVINamePrefix + volumeID,
				Namespace: cnsv1alpha1.CVINamespace,
			},
			Spec: cnsv1alpha1.CsiVolumeInfoSpec{
				VolumeID:     volumeID,
				PVCName:      ctx.pvc.Name,
				PVCNamespace: namespace,
				PVName:       pvName,
			},
		}
	})
	AfterEach(func() {
		ctx = nil
		pv = nil
		cvi = nil
	})

	validateDelete := func(expectedAllowed bool, expectedReason string) {
		var err error

		ctx.WebhookRequestContext.Obj, err = builder.ToUnstructured(ctx.pvc)
		Expect(err).ToNot(HaveOccurred())

		Expect(ctx.Client.Create(ctx, pv)).To(Succeed())
		Expect(ctx.Client.Create(ctx, cvi)).To(Succeed())

		response := ctx.ValidateDelete(&ctx.WebhookRequestContext)
		Expect(response.Allowed).To(Equal(expectedAllowed))
		if expectedReason != "" {
			Expect(string(response.Result.Reason)).To(ContainSubstring(expectedReason))
		}
	}

	Context("when the PVC is unbound", func() {
		It("should allow delete", func() {
			ctx.pvc.Spec.VolumeName = ""
			var err error
			ctx.WebhookRequestContext.Obj, err = builder.ToUnstructured(ctx.pvc)
			Expect(err).ToNot(HaveOccurred())

			response := ctx.ValidateDelete(&ctx.WebhookRequestContext)
			Expect(response.Allowed).To(BeTrue())
		})
	})

	Context("when there is no CsiVolumeInfo for the volume", func() {
		It("should allow delete", func() {
			cvi = nil
			var err error
			ctx.WebhookRequestContext.Obj, err = builder.ToUnstructured(ctx.pvc)
			Expect(err).ToNot(HaveOccurred())
			Expect(ctx.Client.Create(ctx, pv)).To(Succeed())

			response := ctx.ValidateDelete(&ctx.WebhookRequestContext)
			Expect(response.Allowed).To(BeTrue())
		})
	})

	Context("when the CsiVolumeInfo is VMManaged (dependent volume)", func() {
		It("should deny delete", func() {
			cvi.Status.Ownership = cnsv1alpha1.OwnershipStateVMManaged
			cvi.Spec.VMs = []cnsv1alpha1.VirtualMachineRef{
				{VMName: vmName, DiskMode: cnsv1alpha1.CVIDiskModePersistent, VolumeName: "vol1"},
			}
			validateDelete(false, "volume is VM-managed")
		})
	})

	Context("when the CsiVolumeInfo has an attached independent volume (CSIManaged, spec.vms non-empty)", func() {
		It("should deny delete", func() {
			cvi.Status.Ownership = cnsv1alpha1.OwnershipStateCSIManaged
			cvi.Spec.VMs = []cnsv1alpha1.VirtualMachineRef{
				{VMName: vmName, DiskMode: cnsv1alpha1.CVIDiskModeIndependentPersistent, VolumeName: "vol1"},
			}
			validateDelete(false, "volume is attached to a VM-owned VM")
		})
	})

	Context("when the CsiVolumeInfo is CSIManaged with no attached VMs", func() {
		It("should allow delete", func() {
			cvi.Status.Ownership = cnsv1alpha1.OwnershipStateCSIManaged
			validateDelete(true, "")
		})
	})
}
