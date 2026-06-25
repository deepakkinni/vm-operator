// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package volumeattachdetach_test

import (
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha6"
	"github.com/vmware-tanzu/vm-operator/controllers/virtualmachine/volumeattachdetach"
	cnsv1alpha1 "github.com/vmware-tanzu/vm-operator/external/vsphere-csi-driver/api/v1alpha1"
	pkgcfg "github.com/vmware-tanzu/vm-operator/pkg/config"
	pkgconst "github.com/vmware-tanzu/vm-operator/pkg/constants"
	"github.com/vmware-tanzu/vm-operator/pkg/constants/testlabels"
	pkgctx "github.com/vmware-tanzu/vm-operator/pkg/context"
	pkgerr "github.com/vmware-tanzu/vm-operator/pkg/errors"
	"github.com/vmware-tanzu/vm-operator/pkg/util/ptr"
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
			reconciler  *volumeattachdetach.Reconciler
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
					BiosUUID:     "bios-uuid-1234",
					InstanceUUID: "instance-uuid-1234",
					// Hardware must be non-nil: the batch path in ReconcileNormal
					// iterates Status.Hardware.Controllers.
					Hardware: &vmopv1.VirtualMachineHardwareStatus{},
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

			reconciler = volumeattachdetach.NewReconciler(
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
					// StorageClassName must be non-nil so the batch controller's
					// handlePVCWithWFFC does not error. An empty string means "no
					// storage class" and the WFFC check exits early.
					StorageClassName: ptr.To(""),
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
							cvi.Spec.DiskUUID = "disk-uuid-abc"
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

						It("attaches the disk and adds a status entry with Attached=true", func() {
							err := reconciler.ReconcileNormal(volCtx)
							Expect(err).ToNot(HaveOccurred())
							Expect(vm.Status.Volumes).To(HaveLen(1))
							Expect(vm.Status.Volumes[0].Name).To(Equal("vol-persistent"))
							Expect(vm.Status.Volumes[0].Type).To(Equal(vmopv1.VolumeTypeManaged))
							Expect(vm.Status.Volumes[0].DiskUUID).To(Equal("disk-uuid-abc"))
							Expect(vm.Status.Volumes[0].Attached).To(BeTrue(),
								"Attached must be true after attach so reconcileVolumes does not block power-on")
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

					When("VM has multiple dependent volumes, none with a VM entry in their CVI", func() {
						// Regression guard: old code patched only the first CVI and
						// returned early, leaving the rest unpatched until the next
						// reconcile. The new code must patch all in one pass.
						const (
							pvcName2 = "pvc-2"
							pvName2  = "pv-2"
							volID2   = "vol-id-def456"
						)

						BeforeEach(func() {
							pvc1, pv1, cvi1 := buildPVCWithCVI(pvcName, pvName, volID)
							pvc2, pv2, cvi2 := buildPVCWithCVI(pvcName2, pvName2, volID2)
							// Neither CVI has a VM entry yet.
							initObjects = append(initObjects, pvc1, pv1, cvi1, pvc2, pv2, cvi2)

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
								{
									Name: "vol-2",
									VirtualMachineVolumeSource: vmopv1.VirtualMachineVolumeSource{
										PersistentVolumeClaim: &vmopv1.PersistentVolumeClaimVolumeSource{
											PersistentVolumeClaimVolumeSource: corev1.PersistentVolumeClaimVolumeSource{
												ClaimName: pvcName2,
											},
										},
									},
								},
							}
						})

						It("patches all CVIs and returns a RequeueError", func() {
							err := reconciler.ReconcileNormal(volCtx)

							// Must requeue — CSI has not yet set the green signal.
							var requeue pkgerr.RequeueError
							Expect(errors.As(err, &requeue)).To(BeTrue(),
								"expected a RequeueError, got: %v", err)

							// Both CVIs must have the VM entry — not just the first one.
							cvi1 := &cnsv1alpha1.CsiVolumeInfo{}
							Expect(ctx.Client.Get(ctx, client.ObjectKey{
								Name:      vmopv1util.CVINameForVolumeID(volID),
								Namespace: pkgconst.CVISystemNamespace,
							}, cvi1)).To(Succeed())
							Expect(vmopv1util.HasVMEntry(cvi1, vm.Name)).To(BeTrue(),
								"CVI for vol-1 should have VM entry")

							cvi2 := &cnsv1alpha1.CsiVolumeInfo{}
							Expect(ctx.Client.Get(ctx, client.ObjectKey{
								Name:      vmopv1util.CVINameForVolumeID(volID2),
								Namespace: pkgconst.CVISystemNamespace,
							}, cvi2)).To(Succeed())
							Expect(vmopv1util.HasVMEntry(cvi2, vm.Name)).To(BeTrue(),
								"CVI for vol-2 should have VM entry")
						})
					})

					When("VM has multiple dependent volumes, all green with VM entries present", func() {
						const (
							pvcName2 = "pvc-2"
							pvName2  = "pv-2"
							volID2   = "vol-id-def456"
						)

						BeforeEach(func() {
							pvc1, pv1, cvi1 := buildPVCWithCVI(pvcName, pvName, volID)
							pvc2, pv2, cvi2 := buildPVCWithCVI(pvcName2, pvName2, volID2)
							// Pre-set VM entries and distinct DiskUUIDs on both CVIs.
							cvi1.Spec.VMs = []cnsv1alpha1.CsiVolumeInfoVMEntry{{VMName: vm.Name}}
							cvi1.Spec.DiskUUID = "disk-uuid-111"
							cvi2.Spec.VMs = []cnsv1alpha1.CsiVolumeInfoVMEntry{{VMName: vm.Name}}
							cvi2.Spec.DiskUUID = "disk-uuid-222"
							initObjects = append(initObjects, pvc1, pv1, cvi1, pvc2, pv2, cvi2)

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
								{
									Name: "vol-2",
									VirtualMachineVolumeSource: vmopv1.VirtualMachineVolumeSource{
										PersistentVolumeClaim: &vmopv1.PersistentVolumeClaimVolumeSource{
											PersistentVolumeClaimVolumeSource: corev1.PersistentVolumeClaimVolumeSource{
												ClaimName: pvcName2,
											},
										},
									},
								},
							}
						})

						It("attaches all disks and returns nil", func() {
							err := reconciler.ReconcileNormal(volCtx)
							Expect(err).ToNot(HaveOccurred())

							Expect(vm.Status.Volumes).To(HaveLen(2))
							names := []string{
								vm.Status.Volumes[0].Name,
								vm.Status.Volumes[1].Name,
							}
							Expect(names).To(ConsistOf("vol-1", "vol-2"))
							for _, v := range vm.Status.Volumes {
								Expect(v.Type).To(Equal(vmopv1.VolumeTypeManaged))
								Expect(v.DiskUUID).ToNot(BeEmpty())
								Expect(v.Attached).To(BeTrue())
							}
						})
					})

					When("VM has a mix: one green volume and one missing VM entry", func() {
						// The earlier not-ready volume must not block the later green one.
						const (
							pvcName2 = "pvc-2"
							pvName2  = "pv-2"
							volID2   = "vol-id-def456"
						)

						BeforeEach(func() {
							pvc1, pv1, cvi1 := buildPVCWithCVI(pvcName, pvName, volID)
							pvc2, pv2, cvi2 := buildPVCWithCVI(pvcName2, pvName2, volID2)
							// vol-1: no VM entry (needs patching, not yet green).
							// vol-2: VM entry present, green signal already set.
							cvi2.Spec.VMs = []cnsv1alpha1.CsiVolumeInfoVMEntry{{VMName: vm.Name}}
							cvi2.Spec.DiskUUID = "disk-uuid-222"
							initObjects = append(initObjects, pvc1, pv1, cvi1, pvc2, pv2, cvi2)

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
								{
									Name: "vol-2",
									VirtualMachineVolumeSource: vmopv1.VirtualMachineVolumeSource{
										PersistentVolumeClaim: &vmopv1.PersistentVolumeClaimVolumeSource{
											PersistentVolumeClaimVolumeSource: corev1.PersistentVolumeClaimVolumeSource{
												ClaimName: pvcName2,
											},
										},
									},
								},
							}
						})

						It("attaches the green volume, patches the pending CVI, and requeues", func() {
							err := reconciler.ReconcileNormal(volCtx)

							// Must requeue because vol-1 is still pending.
							var requeue pkgerr.RequeueError
							Expect(errors.As(err, &requeue)).To(BeTrue(),
								"expected a RequeueError, got: %v", err)

							// vol-2 must be attached despite vol-1 not being ready.
							Expect(vm.Status.Volumes).To(HaveLen(1))
							Expect(vm.Status.Volumes[0].Name).To(Equal("vol-2"))
							Expect(vm.Status.Volumes[0].DiskUUID).To(Equal("disk-uuid-222"))
							Expect(vm.Status.Volumes[0].Attached).To(BeTrue())

							// CVI for vol-1 must have been patched with the VM entry.
							cvi1 := &cnsv1alpha1.CsiVolumeInfo{}
							Expect(ctx.Client.Get(ctx, client.ObjectKey{
								Name:      vmopv1util.CVINameForVolumeID(volID),
								Namespace: pkgconst.CVISystemNamespace,
							}, cvi1)).To(Succeed())
							Expect(vmopv1util.HasVMEntry(cvi1, vm.Name)).To(BeTrue(),
								"CVI for vol-1 should have VM entry after patch")
						})
					})
				})
			})
		})
	},
)
