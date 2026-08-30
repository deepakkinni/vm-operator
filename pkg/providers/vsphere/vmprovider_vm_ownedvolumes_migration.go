// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package vsphere

import (
	"context"
	"fmt"

	vimtypes "github.com/vmware/govmomi/vim25/types"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha6"
	pkgctx "github.com/vmware-tanzu/vm-operator/pkg/context"
	ctxop "github.com/vmware-tanzu/vm-operator/pkg/context/operation"
	"github.com/vmware-tanzu/vm-operator/pkg/providers"
	res "github.com/vmware-tanzu/vm-operator/pkg/providers/vsphere/resources"
)

// HasAnySnapshot reports whether the VM has any vSphere snapshot, managed or
// unmanaged. Migration's VKS disk-mode conversion (§4.5) must precheck this
// itself: the host's own rejection of a disk-mode change on a snapshotted VM
// carries no property path or device index, so it is useless for telling an
// operator what blocked the conversion.
func (vs *vSphereVMProvider) HasAnySnapshot(
	ctx context.Context,
	vm *vmopv1.VirtualMachine) (bool, error) {

	vmCtx := pkgctx.NewVirtualMachineContext(
		pkgctx.WithVCOpID(ctx, vm, "hasAnySnapshot"),
		vm,
	)

	client, err := vs.getVcClient(vmCtx)
	if err != nil {
		return false, fmt.Errorf("failed to get vCenter client: %w", err)
	}

	vcVM, err := vs.getVM(vmCtx, client, true)
	if err != nil {
		return false, fmt.Errorf("failed to get VM from vCenter: %w", err)
	}

	resVM := res.NewVMFromObject(vcVM)

	moVM, err := resVM.GetProperties(vmCtx, []string{"snapshot"})
	if err != nil {
		return false, fmt.Errorf("failed to get VM snapshot info: %w", err)
	}

	return moVM.Snapshot != nil && len(moVM.Snapshot.RootSnapshotList) > 0, nil
}

// ConvertDisksToIndependentPersistent reconfigures every given disk, in a
// single ReconfigVM_Task, to VirtualDiskMode independent_persistent. This
// edits each existing device in place (migration §4.5 step 2): it is not a
// device add, so it needs no vDiskId and disturbs no CBT state, but it is
// still a ReconfigVM_Task on a live, running VM, and the VM-level CBT
// prohibition (never set changeTrackingEnabled on a migrated VM's
// reconfigure, attach/detach §5.6) still binds across the whole batch.
func (vs *vSphereVMProvider) ConvertDisksToIndependentPersistent(
	ctx context.Context,
	vm *vmopv1.VirtualMachine,
	slots []providers.VolumeDiskModeSlot) error {

	if len(slots) == 0 {
		return nil
	}

	ctx = ctxop.WithContext(ctx)

	vmCtx := pkgctx.NewVirtualMachineContext(
		pkgctx.WithVCOpID(ctx, vm, "convertDisksToIndependentPersistent"),
		vm,
	)

	client, err := vs.getVcClient(vmCtx)
	if err != nil {
		return fmt.Errorf("failed to get vCenter client: %w", err)
	}

	vcVM, err := vs.getVM(vmCtx, client, true)
	if err != nil {
		return fmt.Errorf("failed to get VM from vCenter: %w", err)
	}

	resVM := res.NewVMFromObject(vcVM)

	moVM, err := resVM.GetProperties(vmCtx, []string{"config.hardware.device"})
	if err != nil {
		return fmt.Errorf("failed to get VM hardware devices: %w", err)
	}

	vimMode, err := diskModeToVim(vmopv1.VolumeDiskModeIndependentPersistent)
	if err != nil {
		return err
	}

	deviceChanges := make([]vimtypes.BaseVirtualDeviceConfigSpec, 0, len(slots))
	for _, slot := range slots {
		disk, err := findVirtualDiskDeviceAtSlot(moVM, slot.ControllerType, slot.ControllerBusNumber, slot.UnitNumber)
		if err != nil {
			return fmt.Errorf("failed to find virtual disk for volume %q at slot: %w", slot.VolumeName, err)
		}

		backing, ok := disk.Backing.(*vimtypes.VirtualDiskFlatVer2BackingInfo)
		if !ok {
			return fmt.Errorf("unexpected disk backing type for volume %q at slot (%s bus=%d unit=%d)",
				slot.VolumeName, slot.ControllerType, slot.ControllerBusNumber, slot.UnitNumber)
		}
		if backing.DiskMode == string(vimMode) {
			// Already converted — a prior attempt's reconfigure landed but the
			// caller crashed before recording it. Idempotent no-op.
			continue
		}
		backing.DiskMode = string(vimMode)
		disk.Backing = backing

		deviceChanges = append(deviceChanges, &vimtypes.VirtualDeviceConfigSpec{
			Operation: vimtypes.VirtualDeviceConfigSpecOperationEdit,
			Device:    disk,
		})
	}

	if len(deviceChanges) == 0 {
		return nil
	}

	configSpec := &vimtypes.VirtualMachineConfigSpec{
		DeviceChange: deviceChanges,
	}

	if err := assertNoVMLevelCBT(configSpec); err != nil {
		return err
	}

	_, err = resVM.Reconfigure(vmCtx, configSpec)
	return err
}
