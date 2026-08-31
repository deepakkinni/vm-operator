// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package vmopv1_test

import (
	"context"
	"errors"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha6"
	backupapi "github.com/vmware-tanzu/vm-operator/pkg/backup/api"
	vmopv1util "github.com/vmware-tanzu/vm-operator/pkg/util/vmopv1"
)

// fakeRetentionProvider is a minimal SnapshotRetentionProvider for unit tests.
type fakeRetentionProvider struct {
	pvcDisksBySnap map[string][]backupapi.PVCDiskData
	treeRetained   bool
	treeErr        error
	treeCalled     bool
}

func (f *fakeRetentionProvider) GetPVCDiskDataFromSnapshot(
	_ context.Context, _ *vmopv1.VirtualMachine, snapshotName string) ([]backupapi.PVCDiskData, error) {
	return f.pvcDisksBySnap[snapshotName], nil
}

func (f *fakeRetentionProvider) IsDiskRetainedByAnySnapshot(
	_ context.Context, _ *vmopv1.VirtualMachine, _ string) (bool, error) {
	f.treeCalled = true
	return f.treeRetained, f.treeErr
}

var _ = Describe("IsDiskRetainedBySnapshot", func() {
	const (
		ns       = "test-ns"
		vmName   = "test-vm"
		pvcName  = "pvc-1"
		diskUUID = "6000C29-abc"
	)

	var (
		scheme   *runtime.Scheme
		vm       *vmopv1.VirtualMachine
		provider *fakeRetentionProvider
	)

	BeforeEach(func() {
		scheme = runtime.NewScheme()
		Ω(clientgoscheme.AddToScheme(scheme)).To(Succeed())
		Ω(vmopv1.AddToScheme(scheme)).To(Succeed())

		vm = &vmopv1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: vmName, Namespace: ns},
		}
		provider = &fakeRetentionProvider{
			pvcDisksBySnap: map[string][]backupapi.PVCDiskData{},
		}
	})

	newSnap := func(name string) *vmopv1.VirtualMachineSnapshot {
		return &vmopv1.VirtualMachineSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       vmopv1.VirtualMachineSnapshotSpec{VMName: vmName},
		}
	}

	It("returns true via the managed fast path when another snapshot references the PVC", func() {
		snap := newSnap("snap-2")
		provider.pvcDisksBySnap["snap-2"] = []backupapi.PVCDiskData{{PVCName: pvcName}}

		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(snap).Build()

		retained, err := vmopv1util.IsDiskRetainedBySnapshot(
			context.Background(), c, provider, logr.Discard(), vm, "", pvcName, diskUUID)
		Ω(err).ToNot(HaveOccurred())
		Ω(retained).To(BeTrue())
		// Fast path hit — the vCenter tree backstop must not be consulted.
		Ω(provider.treeCalled).To(BeFalse())
	})

	It("skips the excluded snapshot in the fast path", func() {
		// Only the deleting snapshot references the PVC; it must be excluded.
		snap := newSnap("snap-2")
		provider.pvcDisksBySnap["snap-2"] = []backupapi.PVCDiskData{{PVCName: pvcName}}

		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(snap).Build()

		retained, err := vmopv1util.IsDiskRetainedBySnapshot(
			context.Background(), c, provider, logr.Discard(), vm, "snap-2", pvcName, diskUUID)
		Ω(err).ToNot(HaveOccurred())
		Ω(retained).To(BeFalse())
		// Fast path found nothing → backstop consulted.
		Ω(provider.treeCalled).To(BeTrue())
	})

	It("returns true via the vCenter backstop when no managed snapshot matches", func() {
		snap := newSnap("snap-2")
		provider.pvcDisksBySnap["snap-2"] = []backupapi.PVCDiskData{{PVCName: "some-other-pvc"}}
		provider.treeRetained = true

		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(snap).Build()

		retained, err := vmopv1util.IsDiskRetainedBySnapshot(
			context.Background(), c, provider, logr.Discard(), vm, "", pvcName, diskUUID)
		Ω(err).ToNot(HaveOccurred())
		Ω(retained).To(BeTrue())
		Ω(provider.treeCalled).To(BeTrue())
	})

	It("returns false when neither the fast path nor the backstop retains the disk", func() {
		c := fake.NewClientBuilder().WithScheme(scheme).Build()

		retained, err := vmopv1util.IsDiskRetainedBySnapshot(
			context.Background(), c, provider, logr.Discard(), vm, "", pvcName, diskUUID)
		Ω(err).ToNot(HaveOccurred())
		Ω(retained).To(BeFalse())
		Ω(provider.treeCalled).To(BeTrue())
	})

	It("skips the vCenter backstop when diskUUID is empty", func() {
		c := fake.NewClientBuilder().WithScheme(scheme).Build()

		retained, err := vmopv1util.IsDiskRetainedBySnapshot(
			context.Background(), c, provider, logr.Discard(), vm, "", pvcName, "")
		Ω(err).ToNot(HaveOccurred())
		Ω(retained).To(BeFalse())
		Ω(provider.treeCalled).To(BeFalse())
	})

	It("propagates an error from the vCenter backstop", func() {
		provider.treeErr = errors.New("boom")
		c := fake.NewClientBuilder().WithScheme(scheme).Build()

		_, err := vmopv1util.IsDiskRetainedBySnapshot(
			context.Background(), c, provider, logr.Discard(), vm, "", pvcName, diskUUID)
		Ω(err).To(HaveOccurred())
	})

	It("ignores snapshots that belong to a different VM", func() {
		snap := newSnap("snap-other")
		snap.Spec.VMName = "different-vm"
		provider.pvcDisksBySnap["snap-other"] = []backupapi.PVCDiskData{{PVCName: pvcName}}

		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(snap).Build()

		retained, err := vmopv1util.IsDiskRetainedBySnapshot(
			context.Background(), c, provider, logr.Discard(), vm, "", pvcName, diskUUID)
		Ω(err).ToNot(HaveOccurred())
		// The matching snapshot is for another VM → fast path ignores it,
		// backstop (false) decides.
		Ω(retained).To(BeFalse())
		Ω(provider.treeCalled).To(BeTrue())
	})
})
