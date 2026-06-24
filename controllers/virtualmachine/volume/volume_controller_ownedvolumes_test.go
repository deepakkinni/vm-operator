// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package volume_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha6"
	"github.com/vmware-tanzu/vm-operator/controllers/virtualmachine/volume"
	cnsv1alpha1 "github.com/vmware-tanzu/vm-operator/external/vsphere-csi-driver/api/v1alpha1"
	pkgcfg "github.com/vmware-tanzu/vm-operator/pkg/config"
	pkgconst "github.com/vmware-tanzu/vm-operator/pkg/constants"
	"github.com/vmware-tanzu/vm-operator/pkg/constants/testlabels"
	pkgctx "github.com/vmware-tanzu/vm-operator/pkg/context"
	vmopv1util "github.com/vmware-tanzu/vm-operator/pkg/util/vmopv1"
	"github.com/vmware-tanzu/vm-operator/test/builder"
)

var _ = Describe(
	"VMOwnedVolumes ReconcileNormal",
	Label(testlabels.Controller),
	func() {
		const (
			ns      = "dummy-ns"
			vmName  = "dummy-vm"
			pvName  = "my-pv"
			pvcName = "my-pvc"
			volID   = "vol-id-abc123"
		)

		var (
			initObjects []client.Object
			ctx         *builder.UnitTestContextForController
			reconciler  *volume.Reconciler
			volCtx      *pkgctx.VolumeContext
			vm          *vmopv1.VirtualMachine
		)

		BeforeEach(func() {
			vm = &vmopv1.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{
					Name:      vmName,
					Namespace: ns,
				},
				Status: vmopv1.VirtualMachineStatus{
					BiosUUID: "bios-uuid-1234",
				},
			}
		})

		JustBeforeEach(func() {
			ctx = suite.NewUnitTestContextForController()

			ctx.Client = fake.NewClientBuilder().
				WithScheme(ctx.Client.Scheme()).
				WithObjects(initObjects...).
				WithStatusSubresource(builder.KnownObjectTypes()...).
				WithIndex(
					&cnsv1alpha1.CnsNodeVmAttachment{},
					"spec.nodeuuid",
					func(rawObj client.Object) []string {
						attachment := rawObj.(*cnsv1alpha1.CnsNodeVmAttachment)
						return []string{attachment.Spec.NodeUUID}
					}).
				Build()

			reconciler = volume.NewReconciler(
				ctx,
				ctx.Client,
				ctx.Logger,
				ctx.Recorder,
				ctx.VMProvider,
			)

			volCtx = &pkgctx.VolumeContext{
				Context: ctx,
				Logger:  ctx.Logger,
				VM:      vm,
			}
		})

		AfterEach(func() {
			ctx.AfterEach()
			ctx = nil
			initObjects = nil
			volCtx = nil
			reconciler = nil
		})

		// buildPVCWithCVI creates a bound PVC+PV pair with a CSI volume handle and
		// a matching CsiVolumeInfo with the green signal set and a diskPath populated.
		buildPVCWithCVI := func(pvcNameArg, pvNameArg, volumeID string) (
			*corev1.PersistentVolumeClaim,
			*corev1.PersistentVolume,
			*cnsv1alpha1.CsiVolumeInfo,
		) {
			pvc := &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      pvcNameArg,
					Namespace: ns,
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					VolumeName: pvNameArg,
				},
			}
			pv := &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: pvNameArg,
				},
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeSource: corev1.PersistentVolumeSource{
						CSI: &corev1.CSIPersistentVolumeSource{
							VolumeHandle: volumeID,
						},
					},
				},
			}
			cvi := &cnsv1alpha1.CsiVolumeInfo{
				ObjectMeta: metav1.ObjectMeta{
					Name:      vmopv1util.CVINameForVolumeID(volumeID),
					Namespace: pkgconst.CVISystemNamespace,
				},
				Spec: cnsv1alpha1.CsiVolumeInfoSpec{
					VolumeID: volumeID,
					DiskPath: "/vmfs/volumes/datastore1/disk.vmdk",
				},
				Status: cnsv1alpha1.CsiVolumeInfoStatus{
					Ownership:          cnsv1alpha1.OwnershipVMManaged,
					Phase:              cnsv1alpha1.PhaseSucceeded,
					ObservedGeneration: 0,
				},
			}
			return pvc, pv, cvi
		}

		Context("Feature gate VMOwnedVolumes is disabled", func() {
			JustBeforeEach(func() {
				pkgcfg.SetContext(ctx, func(config *pkgcfg.Config) {
					config.Features.VMOwnedVolumes = false
				})
			})

			When("VM has the vm-owned-volumes annotation", func() {
				BeforeEach(func() {
					vm.Annotations = map[string]string{
						pkgconst.VMOwnedVolumesAnnotation: "true",
					}
					vm.Spec.Volumes = []vmopv1.VirtualMachineVolume{
						{
							Name: "vol-1",
							VirtualMachineVolumeSource: vmopv1.VirtualMachineVolumeSource{
								PersistentVolumeClaim: &vmopv1.PersistentVolumeClaimVolumeSource{
									PersistentVolumeClaimVolumeSource: corev1.PersistentVolumeClaimVolumeSource{
										ClaimName: pvcName,
									},
								},
							},
						},
					}
				})

				It("does not enter the vm-owned volumes path — CVICleanupFinalizer is NOT added", func() {
					// With the feature disabled, ReconcileNormal skips the
					// vm-owned-volumes branch regardless of the annotation.
					_ = reconciler.ReconcileNormal(volCtx)
					Expect(vm.Finalizers).NotTo(ContainElement(pkgconst.CVICleanupFinalizer))
				})
			})
		})

		Context("Feature gate VMOwnedVolumes is enabled", func() {
			JustBeforeEach(func() {
				pkgcfg.SetContext(ctx, func(config *pkgcfg.Config) {
					config.Features.VMOwnedVolumes = true
				})
			})

			When("VM does NOT have the vm-owned-volumes annotation", func() {
				BeforeEach(func() {
					vm.Annotations = nil
					vm.Spec.Volumes = []vmopv1.VirtualMachineVolume{
						{
							Name: "vol-1",
							VirtualMachineVolumeSource: vmopv1.VirtualMachineVolumeSource{
								PersistentVolumeClaim: &vmopv1.PersistentVolumeClaimVolumeSource{
									PersistentVolumeClaimVolumeSource: corev1.PersistentVolumeClaimVolumeSource{
										ClaimName: pvcName,
									},
								},
							},
						},
					}
				})

				It("takes the legacy path — CVICleanupFinalizer is NOT added", func() {
					_ = reconciler.ReconcileNormal(volCtx)
					Expect(vm.Finalizers).NotTo(ContainElement(pkgconst.CVICleanupFinalizer))
				})
			})

			When("VM has the vm-owned-volumes annotation", func() {
				BeforeEach(func() {
					vm.Annotations = map[string]string{
						pkgconst.VMOwnedVolumesAnnotation: "true",
					}
				})

				It("adds CVICleanupFinalizer on first reconcile and returns nil", func() {
					// First reconcile: finalizer is absent → vm-owned volumes path adds it
					// and returns immediately (before any volume work).
					err := reconciler.ReconcileNormal(volCtx)
					Expect(err).ToNot(HaveOccurred())
					Expect(vm.Finalizers).To(ContainElement(pkgconst.CVICleanupFinalizer))
				})

				When("CVICleanupFinalizer is already present", func() {
					BeforeEach(func() {
						vm.Finalizers = []string{pkgconst.CVICleanupFinalizer}
					})

					It("returns nil when spec.volumes is empty", func() {
						err := reconciler.ReconcileNormal(volCtx)
						Expect(err).ToNot(HaveOccurred())
						Expect(vm.Status.Volumes).To(BeEmpty())
					})

					When("VM has a volume with IndependentPersistent diskMode", func() {
						BeforeEach(func() {
							pvc, pv, cvi := buildPVCWithCVI(pvcName, pvName, volID)
							initObjects = append(initObjects, pvc, pv, cvi)

							vm.Spec.Volumes = []vmopv1.VirtualMachineVolume{
								{
									Name:     "vol-independent",
									DiskMode: vmopv1.VolumeDiskModeIndependentPersistent,
									VirtualMachineVolumeSource: vmopv1.VirtualMachineVolumeSource{
										PersistentVolumeClaim: &vmopv1.PersistentVolumeClaimVolumeSource{
											PersistentVolumeClaimVolumeSource: corev1.PersistentVolumeClaimVolumeSource{
												ClaimName: pvcName,
											},
										},
									},
								},
							}
						})

						It("skips the volume — no status entry added", func() {
							err := reconciler.ReconcileNormal(volCtx)
							Expect(err).ToNot(HaveOccurred())
							Expect(vm.Status.Volumes).To(BeEmpty())
						})
					})

					When("VM has a volume with NonPersistent diskMode", func() {
						BeforeEach(func() {
							pvc, pv, cvi := buildPVCWithCVI(pvcName, pvName, volID)
							initObjects = append(initObjects, pvc, pv, cvi)

							vm.Spec.Volumes = []vmopv1.VirtualMachineVolume{
								{
									Name:     "vol-nonpersistent",
									DiskMode: vmopv1.VolumeDiskModeNonPersistent,
									VirtualMachineVolumeSource: vmopv1.VirtualMachineVolumeSource{
										PersistentVolumeClaim: &vmopv1.PersistentVolumeClaimVolumeSource{
											PersistentVolumeClaimVolumeSource: corev1.PersistentVolumeClaimVolumeSource{
												ClaimName: pvcName,
											},
										},
									},
								},
							}
						})

						It("skips the volume — no status entry added", func() {
							err := reconciler.ReconcileNormal(volCtx)
							Expect(err).ToNot(HaveOccurred())
							Expect(vm.Status.Volumes).To(BeEmpty())
						})
					})

					When("VM has a dependent-persistent volume with a green-signal CVI and VM entry present", func() {
						BeforeEach(func() {
							pvc, pv, cvi := buildPVCWithCVI(pvcName, pvName, volID)
							// Pre-set the VM entry so the reconciler skips the patch
							// step and proceeds directly to attach.
							cvi.Spec.VMs = []cnsv1alpha1.CsiVolumeInfoVMEntry{
								{VMName: vmName},
							}
							initObjects = append(initObjects, pvc, pv, cvi)

							vm.Spec.Volumes = []vmopv1.VirtualMachineVolume{
								{
									Name: "vol-persistent",
									// empty DiskMode = Persistent (default)
									VirtualMachineVolumeSource: vmopv1.VirtualMachineVolumeSource{
										PersistentVolumeClaim: &vmopv1.PersistentVolumeClaimVolumeSource{
											PersistentVolumeClaimVolumeSource: corev1.PersistentVolumeClaimVolumeSource{
												ClaimName: pvcName,
											},
										},
									},
								},
							}
						})

						It("attaches the disk and adds a status entry", func() {
							err := reconciler.ReconcileNormal(volCtx)
							Expect(err).ToNot(HaveOccurred())
							Expect(vm.Status.Volumes).To(HaveLen(1))
							Expect(vm.Status.Volumes[0].Name).To(Equal("vol-persistent"))
							Expect(vm.Status.Volumes[0].Type).To(Equal(vmopv1.VolumeTypeManaged))
						})
					})

					When("VM has a dependent-persistent volume but no CVI exists (brownfield PVC)", func() {
						BeforeEach(func() {
							pvc := &corev1.PersistentVolumeClaim{
								ObjectMeta: metav1.ObjectMeta{
									Name:      pvcName,
									Namespace: ns,
								},
								Spec: corev1.PersistentVolumeClaimSpec{
									VolumeName: pvName,
								},
							}
							pv := &corev1.PersistentVolume{
								ObjectMeta: metav1.ObjectMeta{
									Name: pvName,
								},
								Spec: corev1.PersistentVolumeSpec{
									PersistentVolumeSource: corev1.PersistentVolumeSource{
										CSI: &corev1.CSIPersistentVolumeSource{
											VolumeHandle: volID,
										},
									},
								},
							}
							// No CVI: GetCVIForPVC returns not-found → vm-owned-volumes skips.
							initObjects = append(initObjects, pvc, pv)

							vm.Spec.Volumes = []vmopv1.VirtualMachineVolume{
								{
									Name: "vol-brownfield",
									VirtualMachineVolumeSource: vmopv1.VirtualMachineVolumeSource{
										PersistentVolumeClaim: &vmopv1.PersistentVolumeClaimVolumeSource{
											PersistentVolumeClaimVolumeSource: corev1.PersistentVolumeClaimVolumeSource{
												ClaimName: pvcName,
											},
										},
									},
								},
							}
						})

						It("skips the volume and returns nil", func() {
							err := reconciler.ReconcileNormal(volCtx)
							Expect(err).ToNot(HaveOccurred())
							Expect(vm.Status.Volumes).To(BeEmpty())
						})
					})
				})
			})
		})
	},
)
