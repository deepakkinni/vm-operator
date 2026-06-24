// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package vsphere

import (
	"context"
	"fmt"

	"github.com/vmware/govmomi/vim25/mo"
	vimtypes "github.com/vmware/govmomi/vim25/types"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha6"
	pkgctx "github.com/vmware-tanzu/vm-operator/pkg/context"
	res "github.com/vmware-tanzu/vm-operator/pkg/providers/vsphere/resources"
)

// AttachOrphanedDiskToVM adds an existing VMDK (identified by its datastore
// path) to the virtual machine as a plain disk without creating a new virtual
// disk. This is used for the VM-owned volume attach path where the FCD has
// been unregistered and the VMDK must be re-attached to the VM.
func (vs *vSphereVMProvider) AttachOrphanedDiskToVM(
	ctx context.Context,
	vm *vmopv1.VirtualMachine,
	diskPath string) error {

	vmCtx := pkgctx.NewVirtualMachineContext(
		pkgctx.WithVCOpID(ctx, vm, "attachOrphanedDisk"),
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

	backing := &vimtypes.VirtualDiskFlatVer2BackingInfo{
		DiskMode: string(vimtypes.VirtualDiskModePersistent),
	}
	backing.FileName = diskPath

	configSpec := &vimtypes.VirtualMachineConfigSpec{
		DeviceChange: []vimtypes.BaseVirtualDeviceConfigSpec{
			&vimtypes.VirtualDeviceConfigSpec{
				Operation:     vimtypes.VirtualDeviceConfigSpecOperationAdd,
				FileOperation: "", // empty = use existing file, do not create
				Device: &vimtypes.VirtualDisk{
					VirtualDevice: vimtypes.VirtualDevice{
						Backing: backing,
					},
				},
			},
		},
	}

	if _, err := resVM.Reconfigure(vmCtx, configSpec); err != nil {
		return fmt.Errorf("failed to attach orphaned disk %q to VM %q: %w", diskPath, vm.Name, err)
	}

	return nil
}

// DetachDiskAtSlot removes the virtual disk at the given controller slot from
// the virtual machine without deleting the underlying VMDK file. The slot is
// identified by controllerType, controllerBusNumber, and unitNumber as
// recorded in vm.status.volumes. Returns the datastore path of the detached
// disk.
func (vs *vSphereVMProvider) DetachDiskAtSlot(
	ctx context.Context,
	vm *vmopv1.VirtualMachine,
	controllerType vmopv1.VirtualControllerType,
	controllerBusNumber, unitNumber int32) (string, error) {

	vmCtx := pkgctx.NewVirtualMachineContext(
		pkgctx.WithVCOpID(ctx, vm, "detachDiskAtSlot"),
		vm,
	)

	client, err := vs.getVcClient(vmCtx)
	if err != nil {
		return "", fmt.Errorf("failed to get vCenter client: %w", err)
	}

	vcVM, err := vs.getVM(vmCtx, client, true)
	if err != nil {
		return "", fmt.Errorf("failed to get VM from vCenter: %w", err)
	}

	resVM := res.NewVMFromObject(vcVM)

	// Fetch VM hardware devices.
	moVM, err := resVM.GetProperties(vmCtx, []string{"config.hardware.device"})
	if err != nil {
		return "", fmt.Errorf("failed to get VM hardware devices: %w", err)
	}

	disk, diskPath, err := findVirtualDiskAtSlot(moVM, controllerType, controllerBusNumber, unitNumber)
	if err != nil {
		return "", fmt.Errorf("failed to find virtual disk at slot: %w", err)
	}

	configSpec := &vimtypes.VirtualMachineConfigSpec{
		DeviceChange: []vimtypes.BaseVirtualDeviceConfigSpec{
			&vimtypes.VirtualDeviceConfigSpec{
				Operation:     vimtypes.VirtualDeviceConfigSpecOperationRemove,
				FileOperation: "", // empty = preserve the VMDK file
				Device:        disk,
			},
		},
	}

	if _, err := resVM.Reconfigure(vmCtx, configSpec); err != nil {
		return "", fmt.Errorf("failed to detach disk at slot (%s bus=%d unit=%d) from VM %q: %w",
			controllerType, controllerBusNumber, unitNumber, vm.Name, err)
	}

	return diskPath, nil
}

// findVirtualDiskAtSlot locates the VirtualDisk device in the moVM device
// list that is attached to the controller identified by controllerType,
// controllerBusNumber, and unitNumber. It returns the device and its VMDK
// datastore path.
func findVirtualDiskAtSlot(
	moVM *mo.VirtualMachine,
	controllerType vmopv1.VirtualControllerType,
	controllerBusNumber, unitNumber int32) (vimtypes.BaseVirtualDevice, string, error) {

	if moVM.Config == nil {
		return nil, "", fmt.Errorf("VM config is nil")
	}

	// Find the controller device matching the type and bus number.
	var controllerKey int32
	found := false
	for _, dev := range moVM.Config.Hardware.Device {
		bd := dev.GetVirtualDevice()
		if bd == nil {
			continue
		}

		var busNumber int32
		var matched bool

		switch controllerType {
		case vmopv1.VirtualControllerTypeSCSI:
			if c, ok := dev.(vimtypes.BaseVirtualSCSIController); ok {
				busNumber = c.GetVirtualSCSIController().BusNumber
				matched = true
			}
		case vmopv1.VirtualControllerTypeSATA:
			if c, ok := dev.(*vimtypes.VirtualAHCIController); ok {
				busNumber = c.BusNumber
				matched = true
			}
		case vmopv1.VirtualControllerTypeNVME:
			if c, ok := dev.(*vimtypes.VirtualNVMEController); ok {
				busNumber = c.BusNumber
				matched = true
			}
		case vmopv1.VirtualControllerTypeIDE:
			if c, ok := dev.(*vimtypes.VirtualIDEController); ok {
				busNumber = c.BusNumber
				matched = true
			}
		}

		if matched && busNumber == controllerBusNumber {
			controllerKey = bd.Key
			found = true
			break
		}
	}

	if !found {
		return nil, "", fmt.Errorf("controller not found: type=%s busNumber=%d", controllerType, controllerBusNumber)
	}

	// Find the VirtualDisk with the matching controller key and unit number.
	for _, dev := range moVM.Config.Hardware.Device {
		disk, ok := dev.(*vimtypes.VirtualDisk)
		if !ok {
			continue
		}
		bd := disk.GetVirtualDevice()
		if bd.ControllerKey != controllerKey {
			continue
		}
		if bd.UnitNumber == nil || *bd.UnitNumber != unitNumber {
			continue
		}

		// Extract the VMDK path from the backing.
		backing, ok := disk.Backing.(*vimtypes.VirtualDiskFlatVer2BackingInfo)
		if !ok {
			return nil, "", fmt.Errorf("unexpected disk backing type at slot (controller=%d unit=%d)", controllerKey, unitNumber)
		}

		return disk, backing.FileName, nil
	}

	return nil, "", fmt.Errorf("virtual disk not found at slot: controllerKey=%d unitNumber=%d", controllerKey, unitNumber)
}
