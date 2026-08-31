// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package csivolumeinfo_test

import (
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha6"
	"github.com/vmware-tanzu/vm-operator/controllers/csivolumeinfo"
	cnsv1alpha1 "github.com/vmware-tanzu/vm-operator/external/vsphere-csi-driver/api/v1alpha1"
	pkgcfg "github.com/vmware-tanzu/vm-operator/pkg/config"
	"github.com/vmware-tanzu/vm-operator/pkg/record"
)

const (
	testNS  = "test-ns"
	cviName = "cvi-volume-vol-id-1"

	suspectedAtKey  = "vmoperator.vmware.com/orphan-suspected-at"
	suspectedEntKey = "vmoperator.vmware.com/orphan-suspected-entries"
)

func newReconciler(t *testing.T, initObjs ...client.Object) (*csivolumeinfo.Reconciler, client.Client) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := vmopv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := cnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(initObjs...).Build()

	ctx := pkgcfg.NewContextWithDefaultConfig()
	pkgcfg.UpdateContext(ctx, func(config *pkgcfg.Config) {
		config.Features.VMOwnedVolumes = true
	})

	return csivolumeinfo.NewReconciler(ctx, c, c, logr.Discard(), record.New(nil)), c
}

func req() ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: cnsv1alpha1.CVINamespace, Name: cviName}}
}

func getCVI(t *testing.T, c client.Client) *cnsv1alpha1.CsiVolumeInfo {
	t.Helper()
	got := &cnsv1alpha1.CsiVolumeInfo{}
	if err := c.Get(t.Context(), req().NamespacedName, got); err != nil {
		t.Fatalf("failed to get CsiVolumeInfo: %v", err)
	}
	return got
}

func TestReconcile_NoOrphans_NoChange(t *testing.T) {
	cvi := &cnsv1alpha1.CsiVolumeInfo{
		ObjectMeta: metav1.ObjectMeta{Name: cviName, Namespace: cnsv1alpha1.CVINamespace},
		Spec: cnsv1alpha1.CsiVolumeInfoSpec{
			PVCNamespace: testNS,
			VMs:          []cnsv1alpha1.VirtualMachineRef{{VMName: "vm-1"}},
		},
	}
	vm := &vmopv1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "vm-1", Namespace: testNS},
	}

	r, c := newReconciler(t, cvi, vm)

	if _, err := r.Reconcile(t.Context(), req()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := getCVI(t, c)
	if len(got.Spec.VMs) != 1 {
		t.Fatalf("expected the entry to remain, got %+v", got.Spec.VMs)
	}
}

func TestReconcile_OrphanFirstPass_Requeues_NoRemoval(t *testing.T) {
	cvi := &cnsv1alpha1.CsiVolumeInfo{
		ObjectMeta: metav1.ObjectMeta{Name: cviName, Namespace: cnsv1alpha1.CVINamespace},
		Spec: cnsv1alpha1.CsiVolumeInfoSpec{
			PVCNamespace: testNS,
			VMs:          []cnsv1alpha1.VirtualMachineRef{{VMName: "vm-gone"}},
		},
	}

	r, c := newReconciler(t, cvi)

	res, err := r.Reconcile(t.Context(), req())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Fatalf("expected a RequeueAfter on first suspicion, got %v", res)
	}

	got := getCVI(t, c)
	if len(got.Spec.VMs) != 1 {
		t.Fatalf("entry must not be removed on the first pass, got %+v", got.Spec.VMs)
	}
	if got.Annotations[suspectedAtKey] == "" {
		t.Fatal("expected the suspicion timestamp annotation to be stamped")
	}
}

func TestReconcile_OrphanConfirmedAfterGracePeriod_RemovesEntry(t *testing.T) {
	cvi := &cnsv1alpha1.CsiVolumeInfo{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cviName,
			Namespace: cnsv1alpha1.CVINamespace,
			Annotations: map[string]string{
				suspectedAtKey:  time.Now().Add(-10 * time.Minute).Format(time.RFC3339),
				suspectedEntKey: "vm-gone",
			},
		},
		Spec: cnsv1alpha1.CsiVolumeInfoSpec{
			PVCNamespace: testNS,
			VMs:          []cnsv1alpha1.VirtualMachineRef{{VMName: "vm-gone"}},
		},
	}

	r, c := newReconciler(t, cvi)

	if _, err := r.Reconcile(t.Context(), req()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := getCVI(t, c)
	if len(got.Spec.VMs) != 0 {
		t.Fatalf("expected the orphaned entry to be removed, got %+v", got.Spec.VMs)
	}
	if _, ok := got.Annotations[suspectedAtKey]; ok {
		t.Fatal("expected the suspicion annotation to be cleared after acting")
	}
}

func TestReconcile_DifferentOrphanSetRestartsGracePeriod(t *testing.T) {
	cvi := &cnsv1alpha1.CsiVolumeInfo{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cviName,
			Namespace: cnsv1alpha1.CVINamespace,
			Annotations: map[string]string{
				suspectedAtKey:  time.Now().Add(-10 * time.Minute).Format(time.RFC3339),
				suspectedEntKey: "vm-other",
			},
		},
		Spec: cnsv1alpha1.CsiVolumeInfoSpec{
			PVCNamespace: testNS,
			VMs:          []cnsv1alpha1.VirtualMachineRef{{VMName: "vm-gone"}},
		},
	}

	r, c := newReconciler(t, cvi)

	res, err := r.Reconcile(t.Context(), req())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Fatalf("expected the grace period to restart for a changed orphan set, got %v", res)
	}

	got := getCVI(t, c)
	if len(got.Spec.VMs) != 1 {
		t.Fatalf("entry must not be removed when the orphan set changed, got %+v", got.Spec.VMs)
	}
}
