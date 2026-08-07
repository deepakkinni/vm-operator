// © Broadcom. All Rights Reserved.
// The term “Broadcom” refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package validation

import (
	"fmt"
	"reflect"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmgr "sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	cnsv1alpha1 "github.com/vmware-tanzu/vm-operator/external/vsphere-csi-driver/api/v1alpha1"
	"github.com/vmware-tanzu/vm-operator/pkg/builder"
	pkgcfg "github.com/vmware-tanzu/vm-operator/pkg/config"
	pkgctx "github.com/vmware-tanzu/vm-operator/pkg/context"
	"github.com/vmware-tanzu/vm-operator/pkg/providers/vsphere/constants"
	"github.com/vmware-tanzu/vm-operator/webhooks/common"
)

const (
	webHookName = "default"

	operationNotAllowedOnPVC    = "%s operation on PVC with instance storage label is not allowed"
	addingISLabelNotAllowed     = "adding instance storage label is not allowed"
	vmManagedPVCDeleteDeniedFmt = "cannot delete PVC %s: volume is VM-managed; detach from all VMs and delete retaining snapshots first"
)

// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get
// +kubebuilder:rbac:groups=cns.vmware.com,resources=csivolumeinfos,verbs=get

var (
	labelPath                            = field.NewPath("metadata", "labels").Key(constants.InstanceStorageLabelKey)
	allowedAccountsForInstanceStoragePVC = map[string]struct{}{
		"system:serviceaccount:kube-system:persistent-volume-binder":     {},
		"system:serviceaccount:kube-system:pvc-protection-controller":    {},
		"system:serviceaccount:kube-system:generic-garbage-collector":    {},
		"system:serviceaccount:kube-system:namespace-controller":         {},
		"system:serviceaccount:vmware-system-csi:vsphere-csi-controller": {},
	}
)

// +kubebuilder:webhook:verbs=create;update;delete,path=/default-validate--v1-persistentvolumeclaim,mutating=false,failurePolicy=fail,groups="",resources=persistentvolumeclaims,versions=v1,name=default.validating.persistentvolumeclaim.vmoperator.vmware.com,sideEffects=None,admissionReviewVersions=v1

// AddToManager adds the webhook to the provided manager.
func AddToManager(ctx *pkgctx.ControllerManagerContext, mgr ctrlmgr.Manager) error {
	hook, err := builder.NewValidatingWebhook(ctx, mgr, webHookName, NewValidator(mgr.GetClient()))
	if err != nil {
		return fmt.Errorf("failed to create PersistentVolumeClaim validation webhook: %w", err)
	}
	mgr.GetWebhookServer().Register(hook.Path, hook)

	return nil
}

// NewValidator returns the package's Validator.
func NewValidator(client ctrlclient.Client) builder.Validator {
	return validator{
		client: client,
		// TODO BMV Use the Context.scheme instead
		converter: runtime.DefaultUnstructuredConverter,
	}
}

type validator struct {
	client    ctrlclient.Client
	converter runtime.UnstructuredConverter
}

func (v validator) For() schema.GroupVersionKind {
	return corev1.SchemeGroupVersion.WithKind(reflect.TypeOf(corev1.PersistentVolumeClaim{}).Name())
}

/* NOTE: If the user is privileged user, the request will not be validated.*/

func (v validator) ValidateCreate(ctx *pkgctx.WebhookRequestContext) admission.Response {
	if isPrivilegedAccountForISPVC(ctx) {
		return common.BuildValidationResponse(ctx, nil, nil, nil)
	}

	var fieldErrs field.ErrorList
	if isInstanceStorageLabelPresent(ctx.Obj.GetLabels()) {
		fieldErrs = append(fieldErrs, field.Forbidden(labelPath,
			fmt.Sprintf(operationNotAllowedOnPVC, admissionv1.Create)))
	}

	return common.BuildValidationResponse(ctx, nil, convertToStringArray(fieldErrs), nil)
}

func (v validator) ValidateDelete(ctx *pkgctx.WebhookRequestContext) admission.Response {
	if isPrivilegedAccountForISPVC(ctx) {
		return common.BuildValidationResponse(ctx, nil, nil, nil)
	}

	var fieldErrs field.ErrorList
	if isInstanceStorageLabelPresent(ctx.Obj.GetLabels()) {
		fieldErrs = append(fieldErrs, field.Forbidden(labelPath,
			fmt.Sprintf(operationNotAllowedOnPVC, admissionv1.Delete)))
	}

	// Block deletion of PVCs whose associated CsiVolumeInfo has VMManaged
	// ownership, unless the VMOwnedVolumes feature gate is disabled.
	if pkgcfg.FromContext(ctx).Features.VMOwnedVolumes {
		if denied, msg := v.isVMOwnedPVCDeleteDenied(ctx); denied {
			fieldErrs = append(fieldErrs, field.Forbidden(field.NewPath("spec"), msg))
		}
	}

	return common.BuildValidationResponse(ctx, nil, convertToStringArray(fieldErrs), nil)
}

// isVMOwnedPVCDeleteDenied returns true with a denial message when the PVC is
// backed by a CsiVolumeInfo that has status.ownership == VMManaged.
func (v validator) isVMOwnedPVCDeleteDenied(ctx *pkgctx.WebhookRequestContext) (bool, string) {
	// ctx.Obj is *unstructured.Unstructured. Extract the fields we need.
	pvcName := ctx.Obj.GetName()

	// spec.volumeName is the bound PV name.
	pvName, _, _ := unstructured.NestedString(ctx.Obj.Object, "spec", "volumeName")
	if pvName == "" {
		// Unbound PVC — no volume handle to look up.
		return false, ""
	}

	// Get the PV to extract the CSI volume handle.
	pv := &corev1.PersistentVolume{}
	if err := v.client.Get(ctx, ctrlclient.ObjectKey{Name: pvName}, pv); err != nil {
		if apierrors.IsNotFound(err) {
			return false, ""
		}
		ctx.Logger.Error(err, "failed to get PV for VM-owned PVC check", "pv", pvName)
		return false, ""
	}

	if pv.Spec.CSI == nil || pv.Spec.CSI.VolumeHandle == "" {
		return false, ""
	}

	volumeID := pv.Spec.CSI.VolumeHandle
	cviName := cnsv1alpha1.CVINamePrefix + volumeID

	cvi := &cnsv1alpha1.CsiVolumeInfo{}
	if err := v.client.Get(ctx, ctrlclient.ObjectKey{
		Namespace: cnsv1alpha1.CVINamespace,
		Name:      cviName,
	}, cvi); err != nil {
		if apierrors.IsNotFound(err) {
			// No CsiVolumeInfo tracking this volume — allow.
			return false, ""
		}
		ctx.Logger.Error(err, "failed to get CsiVolumeInfo for VM-owned PVC check",
			"cvi", cviName)
		return false, ""
	}

	if cvi.Status.Ownership == cnsv1alpha1.OwnershipStateVMManaged {
		return true, fmt.Sprintf(vmManagedPVCDeleteDeniedFmt, pvcName)
	}

	return false, ""
}

func (v validator) ValidateUpdate(ctx *pkgctx.WebhookRequestContext) admission.Response {
	if isPrivilegedAccountForISPVC(ctx) {
		return common.BuildValidationResponse(ctx, nil, nil, nil)
	}
	var fieldErrs field.ErrorList
	// If instance storage labels already exists for resource, do not allow update resource
	if isInstanceStorageLabelPresent(ctx.OldObj.GetLabels()) {
		fieldErrs = append(fieldErrs, field.Forbidden(labelPath,
			fmt.Sprintf(operationNotAllowedOnPVC, admissionv1.Update)))
	} else if isInstanceStorageLabelPresent(ctx.Obj.GetLabels()) {
		fieldErrs = append(fieldErrs, field.Forbidden(labelPath, addingISLabelNotAllowed))
	}

	return common.BuildValidationResponse(ctx, nil, convertToStringArray(fieldErrs), nil)
}

// isInstanceStorageLabelPresent - returns true/false depending on presence of instance storage label.
func isInstanceStorageLabelPresent(labels map[string]string) bool {
	_, isLabelPresent := labels[constants.InstanceStorageLabelKey]
	return isLabelPresent
}

// convertToStringArray converts field.ErrorList to array of strings.
func convertToStringArray(fieldErrs field.ErrorList) []string {
	validationErrs := make([]string, 0, len(fieldErrs))
	for _, fieldErr := range fieldErrs {
		validationErrs = append(validationErrs, fieldErr.Error())
	}
	return validationErrs
}

// isPrivilegedAccountForISPVC returns true if requested user is privileged to add/modify/delete instance storage PVCs
// As PVC is kubernetes native object, it is managed by few kube system service accounts.
// For instance storage PVC apart from kube system service accounts we also allow
// kube-admin and vm-operator's pod service account to manage these PVCs
// more info - https://kubernetes.io/docs/concepts/storage/persistent-volumes/#lifecycle-of-a-volume-and-claim
// TODO: Dynamically get service accounts which manages PVC.
func isPrivilegedAccountForISPVC(ctx *pkgctx.WebhookRequestContext) bool {
	// ctx.IsPrivilegedAccount returns true is requested user is kube-admin or vm-operator's pods system account.
	if ctx.IsPrivilegedAccount {
		return true
	}

	if _, ok := allowedAccountsForInstanceStoragePVC[ctx.UserInfo.Username]; ok {
		return true
	}

	return false
}
