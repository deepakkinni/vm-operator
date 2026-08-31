// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package volumeattachdetach_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	"github.com/vmware-tanzu/vm-operator/pkg/providers"
	providerfake "github.com/vmware-tanzu/vm-operator/pkg/providers/fake"
	pkgutil "github.com/vmware-tanzu/vm-operator/pkg/util"
	"github.com/vmware-tanzu/vm-operator/pkg/util/ptr"
	vmopv1util "github.com/vmware-tanzu/vm-operator/pkg/util/vmopv1"
	"github.com/vmware-tanzu/vm-operator/test/builder"
)

var _ = Describe(
	"Migration ReconcileNormal",
	Label(testlabels.Controller),
	func() {
		const (
			ns       = "migration-ns"
			vmName   = "migration-vm"
			pvcName  = "migration-pvc"
			pvName   = "migration-pv"
			volID    = "migration-vol-id"
			volName1 = "vol1"

			// A second, pending-attach volume used to give the migration
			// candidacy check a genuine edge to fire on (migration §4.1):
			// present in vm.Spec.Volumes but never added to the batch
			// attachment, so it is the volume that triggers migration while
			// volName1 is what gets migrated. Per the ordering rule it is
			// excluded from the migration batch itself.
			pvcName2 = "migration-pvc-2"
			volName2 = "vol2"
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
					UID:       "migration-vm-uid",
				},
				Status: vmopv1.VirtualMachineStatus{
					BiosUUID:     "bios-uuid-migration",
					InstanceUUID: "instance-uuid-migration",
					Hardware:     &vmopv1.VirtualMachineHardwareStatus{},
				},
			}
			initObjects = nil
		})

		JustBeforeEach(func() {
			ctx = suite.NewUnitTestContextForController()

			pkgcfg.SetContext(ctx, func(config *pkgcfg.Config) {
				config.Features.VMOwnedVolumes = true
			})

			// reconcileMigration patches the VM and CnsNodeVMBatchAttachment
			// directly (not just the in-memory ctx.VM the way the rest of
			// this reconciler's tests do), so both must round-trip through
			// the fake client's object store.
			initObjects = append(initObjects, vm)

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
				WithIndex(
					&cnsv1alpha1.CsiVolumeInfo{},
					vmopv1util.CVIVMInstanceUUIDIndexKey,
					vmopv1util.IndexCVIByVMInstanceUUID).
				WithIndex(
					&cnsv1alpha1.CsiVolumeInfo{},
					vmopv1util.CVIVMNameIndexKey,
					vmopv1util.IndexCVIByVMName).
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

		getBA := func() *cnsv1alpha1.CnsNodeVMBatchAttachment {
			ba := &cnsv1alpha1.CnsNodeVMBatchAttachment{}
			err := ctx.Client.Get(ctx, client.ObjectKey{Name: vmName, Namespace: ns}, ba)
			if apierrors.IsNotFound(err) {
				return nil
			}
			Expect(err).ToNot(HaveOccurred())
			return ba
		}

		// pendingAttachVolume returns a PVC-backed volume deliberately left
		// out of any batch attachment/legacy CR fixture, so that including
		// it in vm.Spec.Volumes is a genuine pending attach -- the edge
		// isMigrationCandidate now requires (migration §4.1).
		pendingAttachVolume := func() vmopv1.VirtualMachineVolume {
			return vmopv1.VirtualMachineVolume{
				Name: volName2,
				VirtualMachineVolumeSource: vmopv1.VirtualMachineVolumeSource{
					PersistentVolumeClaim: &vmopv1.PersistentVolumeClaimVolumeSource{
						PersistentVolumeClaimVolumeSource: corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvcName2,
						},
					},
				},
			}
		}

		getCVI := func() *cnsv1alpha1.CsiVolumeInfo {
			cvi := &cnsv1alpha1.CsiVolumeInfo{}
			err := ctx.Client.Get(ctx, client.ObjectKey{
				Namespace: cnsv1alpha1.CVINamespace,
				Name:      vmopv1util.CVINameForVolumeID(volID),
			}, cvi)
			Expect(err).ToNot(HaveOccurred())
			return cvi
		}

		Context("VM lacks the vm-owned-volumes annotation and has a genuine pending attach (lazy, edge-triggered)", func() {
			BeforeEach(func() {
				vm.Spec.Volumes = []vmopv1.VirtualMachineVolume{
					{
						Name: volName1,
						VirtualMachineVolumeSource: vmopv1.VirtualMachineVolumeSource{
							PersistentVolumeClaim: &vmopv1.PersistentVolumeClaimVolumeSource{
								PersistentVolumeClaimVolumeSource: corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: pvcName,
								},
							},
						},
					},
					pendingAttachVolume(),
				}

				pvc := &corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{Name: pvcName, Namespace: ns},
					Spec: corev1.PersistentVolumeClaimSpec{
						VolumeName:       pvName,
						StorageClassName: ptr.To(""),
					},
				}
				pv := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{Name: pvName},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{VolumeHandle: volID},
						},
					},
				}
				// Only volName1 is tracked by the batch attachment -- volName2
				// (pendingAttachVolume) is what makes this VM a migration
				// candidate; it must not appear in ba.Spec.Volumes here.
				ba := cnsBatchAttachmentForVMVolume(vm, vm.Spec.Volumes[:1])
				ba.Spec.Volumes[0].PersistentVolumeClaim.DiskMode = cnsv1alpha1.Persistent

				initObjects = append(initObjects, pvc, pv, ba)
			})

			It("freezes the BA, writes the CVI entry, removes the disk from BA.spec, and requeues", func() {
				err := reconciler.ReconcileNormal(volCtx)
				Expect(err).To(HaveOccurred())
				Expect(pkgerr.IsRequeueError(err)).To(BeTrue())

				ba := getBA()
				Expect(ba).ToNot(BeNil())
				Expect(ba.Annotations[pkgconst.VMOwnedMigrationAnnotation]).To(Equal(pkgconst.VMOwnedMigrationInProgress))
				Expect(ba.Spec.Volumes).To(BeEmpty())

				cvi := getCVI()
				entry := vmopv1util.VMEntry(cvi, vmName)
				Expect(entry).ToNot(BeNil())
				Expect(entry.DiskMode).To(Equal(cnsv1alpha1.CVIDiskModePersistent))
				Expect(entry.VolumeName).To(Equal(volName1))
				Expect(entry.VMInstanceUUID).To(Equal(vm.Status.InstanceUUID))

				Expect(vmopv1util.HasVMOwnedVolumesAnnotation(vm)).To(BeFalse())
			})

			It("completes migration once the dependent CVI reaches VMManaged", func() {
				// First pass: freeze + write entry + remove from BA.spec.
				_ = reconciler.ReconcileNormal(volCtx)

				// Simulate CSI completing the ownership transfer.
				cvi := getCVI()
				cvi.Status.Ownership = cnsv1alpha1.OwnershipStateVMManaged
				Expect(ctx.Client.Update(ctx, cvi)).To(Succeed())

				err := reconciler.ReconcileNormal(volCtx)
				Expect(err).ToNot(HaveOccurred())

				Expect(vmopv1util.HasVMOwnedVolumesAnnotation(vm)).To(BeTrue())
				Expect(getBA()).To(BeNil(), "BA should be deleted once migration completes")
			})

			It("treats a deferred (fcd-retained) dependent disk as migrated", func() {
				_ = reconciler.ReconcileNormal(volCtx)

				cvi := getCVI()
				cvi.Status.Ownership = cnsv1alpha1.OwnershipStateVMManaged
				metav1.SetMetaDataAnnotation(&cvi.ObjectMeta, cnsv1alpha1.FcdRetainedAnnotation, "true")
				Expect(ctx.Client.Update(ctx, cvi)).To(Succeed())

				err := reconciler.ReconcileNormal(volCtx)
				Expect(err).ToNot(HaveOccurred())
				Expect(vmopv1util.HasVMOwnedVolumesAnnotation(vm)).To(BeTrue())
			})
		})

		Context("independent disk", func() {
			BeforeEach(func() {
				vm.Spec.Volumes = []vmopv1.VirtualMachineVolume{
					{
						Name:     volName1,
						DiskMode: vmopv1.VolumeDiskModeIndependentPersistent,
						VirtualMachineVolumeSource: vmopv1.VirtualMachineVolumeSource{
							PersistentVolumeClaim: &vmopv1.PersistentVolumeClaimVolumeSource{
								PersistentVolumeClaimVolumeSource: corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: pvcName,
								},
							},
						},
					},
					pendingAttachVolume(),
				}

				pvc := &corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{Name: pvcName, Namespace: ns},
					Spec: corev1.PersistentVolumeClaimSpec{
						VolumeName:       pvName,
						StorageClassName: ptr.To(""),
					},
				}
				pv := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{Name: pvName},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{VolumeHandle: volID},
						},
					},
				}
				ba := cnsBatchAttachmentForVMVolume(vm, vm.Spec.Volumes[:1])
				ba.Spec.Volumes[0].PersistentVolumeClaim.DiskMode = cnsv1alpha1.IndependentPersistent

				initObjects = append(initObjects, pvc, pv, ba)
			})

			It("re-homes immediately and completes migration in one reconcile, without waiting on CSI", func() {
				err := reconciler.ReconcileNormal(volCtx)
				Expect(err).ToNot(HaveOccurred())

				cvi := getCVI()
				entry := vmopv1util.VMEntry(cvi, vmName)
				Expect(entry).ToNot(BeNil())
				Expect(entry.DiskMode).To(Equal(cnsv1alpha1.CVIDiskModeIndependentPersistent))
				Expect(cvi.Status.Ownership).ToNot(Equal(cnsv1alpha1.OwnershipStateVMManaged))

				Expect(vmopv1util.HasVMOwnedVolumesAnnotation(vm)).To(BeTrue())
				Expect(getBA()).To(BeNil())
			})
		})

		Context("VM has the explicit migrate-to-vm-owned trigger and no existing BA", func() {
			BeforeEach(func() {
				vm.Annotations = map[string]string{
					pkgconst.MigrateToVMOwnedAnnotation: "true",
				}
			})

			It("flips the annotation immediately with nothing to migrate", func() {
				err := reconciler.ReconcileNormal(volCtx)
				Expect(err).ToNot(HaveOccurred())
				Expect(vmopv1util.HasVMOwnedVolumesAnnotation(vm)).To(BeTrue())
				Expect(getBA()).To(BeNil())
			})
		})

		Context("VM already has the vm-owned-volumes annotation", func() {
			BeforeEach(func() {
				vm.Annotations = map[string]string{
					pkgconst.VMOwnedVolumesAnnotation: "true",
				}
			})

			It("is not treated as a migration candidate", func() {
				err := reconciler.ReconcileNormal(volCtx)
				Expect(err).ToNot(HaveOccurred())
				// No BA was ever created, and reconcileOwnedVolumes runs
				// instead — nothing migration-specific should have happened.
				Expect(getBA()).To(BeNil())
			})
		})

		Context("VKS node VM", func() {
			BeforeEach(func() {
				vm.Labels = map[string]string{
					"capw.vmware.com/cluster.role": "worker",
				}
				vm.Spec.Volumes = []vmopv1.VirtualMachineVolume{
					{
						Name: volName1,
						VirtualMachineVolumeSource: vmopv1.VirtualMachineVolumeSource{
							PersistentVolumeClaim: &vmopv1.PersistentVolumeClaimVolumeSource{
								PersistentVolumeClaimVolumeSource: corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: pvcName,
								},
							},
						},
					},
					pendingAttachVolume(),
				}
				vm.Status.Volumes = []vmopv1.VirtualMachineVolumeStatus{
					{
						Name:                volName1,
						Type:                vmopv1.VolumeTypeManaged,
						ControllerType:      vmopv1.VirtualControllerTypeSCSI,
						ControllerBusNumber: ptr.To(int32(0)),
						UnitNumber:          ptr.To(int32(1)),
					},
				}

				pvc := &corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{Name: pvcName, Namespace: ns},
					Spec: corev1.PersistentVolumeClaimSpec{
						VolumeName:       pvName,
						StorageClassName: ptr.To(""),
					},
				}
				pv := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{Name: pvName},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{VolumeHandle: volID},
						},
					},
				}
				ba := cnsBatchAttachmentForVMVolume(vm, vm.Spec.Volumes[:1])
				ba.Spec.Volumes[0].PersistentVolumeClaim.DiskMode = cnsv1alpha1.Persistent

				initObjects = append(initObjects, pvc, pv, ba)
			})

			It("converts the disk to independent-persistent before re-homing it", func() {
				var convertedSlots []providers.VolumeDiskModeSlot
				ctx.VMProvider.(*providerfake.VMProvider).ConvertDisksToIndependentPersistentFn = func(
					_ context.Context,
					_ *vmopv1.VirtualMachine,
					slots []providers.VolumeDiskModeSlot) error {

					convertedSlots = slots
					return nil
				}

				err := reconciler.ReconcileNormal(volCtx)
				Expect(err).ToNot(HaveOccurred())

				Expect(convertedSlots).To(HaveLen(1))
				Expect(convertedSlots[0].ControllerType).To(Equal(vmopv1.VirtualControllerTypeSCSI))
				Expect(convertedSlots[0].ControllerBusNumber).To(Equal(int32(0)))
				Expect(convertedSlots[0].UnitNumber).To(Equal(int32(1)))

				Expect(vm.Spec.Volumes[0].DiskMode).To(Equal(vmopv1.VolumeDiskModeIndependentPersistent))

				cvi := getCVI()
				entry := vmopv1util.VMEntry(cvi, vmName)
				Expect(entry).ToNot(BeNil())
				Expect(entry.DiskMode).To(Equal(cnsv1alpha1.CVIDiskModeIndependentPersistent))

				Expect(vmopv1util.HasVMOwnedVolumesAnnotation(vm)).To(BeTrue())
			})

			It("stalls the conversion when the VM has a vSphere snapshot", func() {
				ctx.VMProvider.(*providerfake.VMProvider).HasAnySnapshotFn = func(
					_ context.Context,
					_ *vmopv1.VirtualMachine) (bool, error) {
					return true, nil
				}

				err := reconciler.ReconcileNormal(volCtx)
				Expect(err).To(HaveOccurred())
				Expect(pkgerr.IsRequeueError(err)).To(BeTrue())

				// No spec write, no CVI, no annotation flip.
				Expect(vm.Spec.Volumes[0].DiskMode).To(BeEmpty())
				Expect(vmopv1util.HasVMOwnedVolumesAnnotation(vm)).To(BeFalse())
			})
		})

		Context("VM lacks the vm-owned-volumes annotation but has no pending attach/detach (stable)", func() {
			BeforeEach(func() {
				vm.Status.Hardware = &vmopv1.VirtualMachineHardwareStatus{
					Controllers: []vmopv1.VirtualControllerStatus{
						{
							Type:      vmopv1.VirtualControllerTypeSCSI,
							BusNumber: 0,
							DeviceKey: 1000,
						},
					},
				}
				vm.Spec.Volumes = []vmopv1.VirtualMachineVolume{
					{
						Name:                volName1,
						ControllerType:      vmopv1.VirtualControllerTypeSCSI,
						ControllerBusNumber: ptr.To(int32(0)),
						UnitNumber:          ptr.To(int32(0)),
						DiskMode:            vmopv1.VolumeDiskModePersistent,
						SharingMode:         vmopv1.VolumeSharingModeNone,
						VirtualMachineVolumeSource: vmopv1.VirtualMachineVolumeSource{
							PersistentVolumeClaim: &vmopv1.PersistentVolumeClaimVolumeSource{
								PersistentVolumeClaimVolumeSource: corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: pvcName,
								},
							},
						},
					},
				}

				pvc := &corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{Name: pvcName, Namespace: ns},
					Spec: corev1.PersistentVolumeClaimSpec{
						VolumeName:       pvName,
						StorageClassName: ptr.To(""),
					},
					Status: corev1.PersistentVolumeClaimStatus{
						Phase: corev1.ClaimBound,
					},
				}
				pv := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{Name: pvName},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{VolumeHandle: volID},
						},
					},
				}
				// The batch attachment already matches vm.Spec.Volumes
				// exactly -- no pending attach or detach.
				ba := cnsBatchAttachmentForVMVolume(vm, vm.Spec.Volumes)
				ba.Spec.Volumes[0].PersistentVolumeClaim.DiskMode = cnsv1alpha1.Persistent

				initObjects = append(initObjects, pvc, pv, ba)
			})

			It("does not trigger migration, even across repeated reconciles", func() {
				Expect(reconciler.ReconcileNormal(volCtx)).To(Succeed())

				ba := getBA()
				Expect(ba).ToNot(BeNil())
				Expect(ba.Annotations[pkgconst.VMOwnedMigrationAnnotation]).To(BeEmpty())
				Expect(vmopv1util.HasVMOwnedVolumesAnnotation(vm)).To(BeFalse())

				// A second, identical reconcile must not change anything --
				// confirms the trigger is edge-based, not re-fired on every
				// reconcile merely because the VM has a PVC (migration §4.1).
				Expect(reconciler.ReconcileNormal(volCtx)).To(Succeed())
				Expect(vmopv1util.HasVMOwnedVolumesAnnotation(vm)).To(BeFalse())
			})
		})

		Context("VM has a legacy CnsNodeVmAttachment alongside a pending attach elsewhere", func() {
			var legacyAttachmentName string

			BeforeEach(func() {
				legacyAttachmentName = pkgutil.CNSAttachmentNameForVolume(vmName, volName1)

				vm.Spec.Volumes = []vmopv1.VirtualMachineVolume{
					{
						Name: volName1,
						VirtualMachineVolumeSource: vmopv1.VirtualMachineVolumeSource{
							PersistentVolumeClaim: &vmopv1.PersistentVolumeClaimVolumeSource{
								PersistentVolumeClaimVolumeSource: corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: pvcName,
								},
							},
						},
					},
					pendingAttachVolume(),
				}

				pvc := &corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{Name: pvcName, Namespace: ns},
					Spec: corev1.PersistentVolumeClaimSpec{
						VolumeName:       pvName,
						StorageClassName: ptr.To(""),
					},
				}
				pv := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{Name: pvName},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{VolumeHandle: volID},
						},
					},
				}
				// volName1 predates the VM's move to the batch attachment
				// mechanism and is still tracked by a legacy
				// CnsNodeVmAttachment CR, not a BA -- no BA exists at all.
				// The pending attach of volName2 is still what makes this a
				// migration candidate (migration §4.1); volName1 is what
				// gets migrated, via the legacy-CR path.
				legacyAttachment := &cnsv1alpha1.CnsNodeVmAttachment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      legacyAttachmentName,
						Namespace: ns,
					},
					Spec: cnsv1alpha1.CnsNodeVmAttachmentSpec{
						NodeUUID:   vm.Status.BiosUUID,
						VolumeName: pvcName,
					},
					Status: cnsv1alpha1.CnsNodeVmAttachmentStatus{
						Attached: true,
						AttachmentMetadata: map[string]string{
							cnsv1alpha1.AttributeFirstClassDiskUUID: "legacy-disk-uuid",
						},
					},
				}

				initObjects = append(initObjects, pvc, pv, legacyAttachment)
			})

			It("migrates the legacy-tracked disk and deletes its CnsNodeVmAttachment once VMManaged", func() {
				err := reconciler.ReconcileNormal(volCtx)
				Expect(err).To(HaveOccurred())
				Expect(pkgerr.IsRequeueError(err)).To(BeTrue())

				// CVI written for the legacy-tracked disk; the legacy CR
				// still exists pending CSI's ownership transfer.
				cvi := getCVI()
				entry := vmopv1util.VMEntry(cvi, vmName)
				Expect(entry).ToNot(BeNil())
				Expect(entry.DiskMode).To(Equal(cnsv1alpha1.CVIDiskModePersistent))
				Expect(entry.VolumeName).To(Equal(volName1))

				legacyAttachment := &cnsv1alpha1.CnsNodeVmAttachment{}
				Expect(ctx.Client.Get(ctx, client.ObjectKey{Name: legacyAttachmentName, Namespace: ns}, legacyAttachment)).To(Succeed())

				// Simulate CSI completing the ownership transfer.
				cvi.Status.Ownership = cnsv1alpha1.OwnershipStateVMManaged
				Expect(ctx.Client.Update(ctx, cvi)).To(Succeed())

				err = reconciler.ReconcileNormal(volCtx)
				Expect(err).ToNot(HaveOccurred())

				Expect(vmopv1util.HasVMOwnedVolumesAnnotation(vm)).To(BeTrue())

				legacyAttachment = &cnsv1alpha1.CnsNodeVmAttachment{}
				getErr := ctx.Client.Get(ctx, client.ObjectKey{Name: legacyAttachmentName, Namespace: ns}, legacyAttachment)
				Expect(apierrors.IsNotFound(getErr)).To(BeTrue(), "legacy CnsNodeVmAttachment should be deleted once migrated")
			})
		})

		Context("VM's only tracked disk is being detached via a legacy CnsNodeVmAttachment", func() {
			var legacyAttachmentName string

			BeforeEach(func() {
				legacyAttachmentName = pkgutil.CNSAttachmentNameForVolume(vmName, volName1)

				// volName1 is NOT in vm.Spec.Volumes -- it is being detached.
				// That is the pending edge; there is nothing else to migrate.
				vm.Spec.Volumes = nil

				legacyAttachment := &cnsv1alpha1.CnsNodeVmAttachment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      legacyAttachmentName,
						Namespace: ns,
					},
					Spec: cnsv1alpha1.CnsNodeVmAttachmentSpec{
						NodeUUID:   vm.Status.BiosUUID,
						VolumeName: pvcName,
					},
					Status: cnsv1alpha1.CnsNodeVmAttachmentStatus{
						Attached: true,
						AttachmentMetadata: map[string]string{
							cnsv1alpha1.AttributeFirstClassDiskUUID: "legacy-disk-uuid",
						},
					},
				}

				initObjects = append(initObjects, legacyAttachment)
			})

			It("triggers migration on the detach edge and completes with nothing to migrate", func() {
				err := reconciler.ReconcileNormal(volCtx)
				Expect(err).ToNot(HaveOccurred())

				Expect(vmopv1util.HasVMOwnedVolumesAnnotation(vm)).To(BeTrue())
				Expect(getBA()).To(BeNil())
			})
		})
	},
)
