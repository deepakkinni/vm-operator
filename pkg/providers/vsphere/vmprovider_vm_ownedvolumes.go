// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package vsphere

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	vimtypes "github.com/vmware/govmomi/vim25/types"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha6"
	pkgctx "github.com/vmware-tanzu/vm-operator/pkg/context"
	ctxop "github.com/vmware-tanzu/vm-operator/pkg/context/operation"
	res "github.com/vmware-tanzu/vm-operator/pkg/providers/vsphere/resources"
)

// AttachOrphanedDiskToVM adds an existing VMDK (identified by its datastore
// path) to the virtual machine as a plain disk without creating a new virtual
// disk. This is used for the VM-owned volume attach path where the FCD has
// been unregistered and the VMDK must be re-attached to the VM. If a disk
// with the same backing path is already present on the VM the call is a no-op,
// which makes the method safe for imported VMs whose disks arrive pre-attached.
func (vs *vSphereVMProvider) AttachOrphanedDiskToVM(
	ctx context.Context,
	vm *vmopv1.VirtualMachine,
	diskPath string) error {

	// Initialise the operation context so that resVM.Reconfigure can call
	// ctxop.MarkUpdate without panicking. Mirrors backup.go:223.
	ctx = ctxop.WithContext(ctx)

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

	// Fetch current hardware to check whether the disk is already attached.
	moVM, err := resVM.GetProperties(vmCtx, []string{"config.hardware.device"})
	if err != nil {
		return fmt.Errorf("failed to get VM hardware devices: %w", err)
	}
	if moVM.Config != nil {
		for _, dev := range moVM.Config.Hardware.Device {
			disk, ok := dev.(*vimtypes.VirtualDisk)
			if !ok {
				continue
			}
			switch b := disk.Backing.(type) {
			case *vimtypes.VirtualDiskFlatVer2BackingInfo:
				if b.FileName == diskPath {
					return nil
				}
			case *vimtypes.VirtualDiskSeSparseBackingInfo:
				if b.FileName == diskPath {
					return nil
				}
			case *vimtypes.VirtualDiskSparseVer2BackingInfo:
				if b.FileName == diskPath {
					return nil
				}
			}
		}
	}

	backing := &vimtypes.VirtualDiskFlatVer2BackingInfo{
		DiskMode: string(vimtypes.VirtualDiskModePersistent),
	}
	backing.FileName = diskPath

	disk := &vimtypes.VirtualDisk{
		VirtualDevice: vimtypes.VirtualDevice{
			Backing: backing,
		},
	}

	// Assign the disk to an existing disk controller (SCSI by default) so that
	// vSphere accepts the device. Without a controller key and unit number the
	// ReconfigVM call fails with "Device requires a controller".
	devices := object.VirtualDeviceList(moVM.Config.Hardware.Device)
	controller, err := devices.FindDiskController("")
	if err != nil {
		return fmt.Errorf("failed to find a disk controller on VM %q: %w", vm.Name, err)
	}
	devices.AssignController(disk, controller)

	configSpec := &vimtypes.VirtualMachineConfigSpec{
		DeviceChange: []vimtypes.BaseVirtualDeviceConfigSpec{
			&vimtypes.VirtualDeviceConfigSpec{
				Operation:     vimtypes.VirtualDeviceConfigSpecOperationAdd,
				FileOperation: "", // empty = use existing file, do not create
				Device:        disk,
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

	// Initialise the operation context so that resVM.Reconfigure can call
	// ctxop.MarkUpdate without panicking. Mirrors backup.go:223.
	ctx = ctxop.WithContext(ctx)

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

		return disk, rootBackingFileName(backing), nil
	}

	return nil, "", fmt.Errorf("virtual disk not found at slot: controllerKey=%d unitNumber=%d", controllerKey, unitNumber)
}

// rootBackingFileName walks the backing's Parent chain to the root ancestor —
// the base FCD VMDK that predates any snapshot redo-log deltas. When a VM
// has snapshots, the live disk backing has a "-000001" (or higher) suffix;
// only the root file is registerable by CNS via registerDisk.
func rootBackingFileName(b *vimtypes.VirtualDiskFlatVer2BackingInfo) string {
	for b.Parent != nil {
		b = b.Parent
	}
	return normalizeBackingFileName(b.FileName)
}

// normalizeBackingFileName converts a vCenter HTTP folder URL
// (https://host/folder/<path>?dsName=<ds>&dcPath=<dc>) to the canonical
// datastore-path format ([<ds>] <path>) expected by CSI and the ReconfigVM
// attach API. If the input is already a datastore path or does not match the
// folder-URL shape, it is returned unchanged.
func normalizeBackingFileName(fileName string) string {
	if !strings.HasPrefix(fileName, "https://") && !strings.HasPrefix(fileName, "http://") {
		return fileName
	}
	u, err := url.Parse(fileName)
	if err != nil {
		return fileName
	}
	dsName := u.Query().Get("dsName")
	filePath := strings.TrimPrefix(u.Path, "/folder/")
	if dsName == "" || filePath == u.Path {
		return fileName
	}
	return fmt.Sprintf("[%s] %s", dsName, filePath)
}
