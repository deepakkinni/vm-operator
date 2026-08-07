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

// ConvertDiskToIndependentPersistent reconfigures the virtual disk at the
// given controller slot to VirtualDiskMode independent_persistent. This
// edits the existing device in place (migration §4.5 step 2): it is not a
// device add, so it needs no vDiskId and disturbs no CBT state, but it is
// still a ReconfigVM_Task on a live, running VM, and the VM-level CBT
// prohibition (never set changeTrackingEnabled on a migrated VM's
// reconfigure, attach/detach §5.6) still binds.
func (vs *vSphereVMProvider) ConvertDiskToIndependentPersistent(
	ctx context.Context,
	vm *vmopv1.VirtualMachine,
	controllerType vmopv1.VirtualControllerType,
	controllerBusNumber, unitNumber int32) error {

	ctx = ctxop.WithContext(ctx)

	vmCtx := pkgctx.NewVirtualMachineContext(
		pkgctx.WithVCOpID(ctx, vm, "convertDiskToIndependentPersistent"),
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

	disk, err := findVirtualDiskDeviceAtSlot(moVM, controllerType, controllerBusNumber, unitNumber)
	if err != nil {
		return fmt.Errorf("failed to find virtual disk at slot: %w", err)
	}

	backing, ok := disk.Backing.(*vimtypes.VirtualDiskFlatVer2BackingInfo)
	if !ok {
		return fmt.Errorf("unexpected disk backing type at slot (%s bus=%d unit=%d)",
			controllerType, controllerBusNumber, unitNumber)
	}

	vimMode, err := diskModeToVim(vmopv1.VolumeDiskModeIndependentPersistent)
	if err != nil {
		return err
	}
	if backing.DiskMode == string(vimMode) {
		// Already converted — a prior attempt's reconfigure landed but the
		// caller crashed before recording it. Idempotent no-op.
		return nil
	}
	backing.DiskMode = string(vimMode)
	disk.Backing = backing

	configSpec := &vimtypes.VirtualMachineConfigSpec{
		DeviceChange: []vimtypes.BaseVirtualDeviceConfigSpec{
			&vimtypes.VirtualDeviceConfigSpec{
				Operation: vimtypes.VirtualDeviceConfigSpecOperationEdit,
				Device:    disk,
			},
		},
	}

	if err := assertNoVMLevelCBT(configSpec); err != nil {
		return err
	}

	_, err = resVM.Reconfigure(vmCtx, configSpec)
	return err
}
