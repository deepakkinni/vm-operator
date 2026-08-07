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
	"github.com/vmware-tanzu/vm-operator/pkg/providers"
	res "github.com/vmware-tanzu/vm-operator/pkg/providers/vsphere/resources"
)

// AttachVolumeDisks adds each of the given disks to the VM in a single
// ReconfigVM_Task (attach/detach §7.3 note — as few reconfigures as
// possible). A disk already present at its backing path is omitted from the
// device-add request but still reported in the result, so a partially
// applied batch converges on retry.
func (vs *vSphereVMProvider) AttachVolumeDisks(
	ctx context.Context,
	vm *vmopv1.VirtualMachine,
	disks []providers.VolumeDiskAddSpec) ([]providers.VolumeDiskPlacement, error) {

	if len(disks) == 0 {
		return nil, nil
	}

	ctx = ctxop.WithContext(ctx)

	vmCtx := pkgctx.NewVirtualMachineContext(
		pkgctx.WithVCOpID(ctx, vm, "attachVolumeDisks"),
		vm,
	)

	client, err := vs.getVcClient(vmCtx)
	if err != nil {
		return nil, fmt.Errorf("failed to get vCenter client: %w", err)
	}

	vcVM, err := vs.getVM(vmCtx, client, true)
	if err != nil {
		return nil, fmt.Errorf("failed to get VM from vCenter: %w", err)
	}

	resVM := res.NewVMFromObject(vcVM)

	moVM, err := resVM.GetProperties(vmCtx, []string{"config.hardware.device"})
	if err != nil {
		return nil, fmt.Errorf("failed to get VM hardware devices: %w", err)
	}
	if moVM.Config == nil {
		return nil, fmt.Errorf("VM %q config is nil", vm.Name)
	}

	devices := object.VirtualDeviceList(moVM.Config.Hardware.Device)

	placements := make([]providers.VolumeDiskPlacement, len(disks))
	deviceChanges := make([]vimtypes.BaseVirtualDeviceConfigSpec, 0, len(disks))
	pendingIdx := make([]int, 0, len(disks))

	// Resolve every slot before building the spec so a collision between two
	// disks in the same batch is caught here, not by vCenter rejecting the
	// whole call.
	for i, d := range disks {
		if existing := findDiskByBackingPath(devices, d.DiskPath); existing != nil {
			placements[i] = placementFromDisk(devices, d.VolumeName, existing)
			continue
		}

		disk, err := buildVolumeDisk(devices, d)
		if err != nil {
			return nil, fmt.Errorf("failed to build device for volume %q: %w", d.VolumeName, err)
		}

		devices = append(devices, disk)
		deviceChanges = append(deviceChanges, &vimtypes.VirtualDeviceConfigSpec{
			Operation:     vimtypes.VirtualDeviceConfigSpecOperationAdd,
			FileOperation: "", // empty = use existing file, do not create
			Device:        disk,
		})
		pendingIdx = append(pendingIdx, i)
	}

	if len(deviceChanges) == 0 {
		return placements, nil
	}

	configSpec := &vimtypes.VirtualMachineConfigSpec{
		DeviceChange: deviceChanges,
	}

	if _, err := resVM.Reconfigure(vmCtx, configSpec); err != nil {
		return nil, fmt.Errorf("failed to attach %d volume disk(s) to VM %q: %w", len(deviceChanges), vm.Name, err)
	}

	// A.6 verification: read the VM config back so the reported placement
	// carries the observed disk UUID, not the pre-reconfigure placeholder
	// (attach/detach §7.3 A.6).
	moVM, err = resVM.GetProperties(vmCtx, []string{"config.hardware.device"})
	if err != nil {
		return nil, fmt.Errorf("failed to read back VM hardware devices after attach: %w", err)
	}
	devices = object.VirtualDeviceList(moVM.Config.Hardware.Device)

	for _, i := range pendingIdx {
		d := disks[i]
		observed := findDiskByBackingPath(devices, d.DiskPath)
		if observed == nil {
			return nil, fmt.Errorf("disk for volume %q not found on VM %q after reconfigure", d.VolumeName, vm.Name)
		}
		placements[i] = placementFromDisk(devices, d.VolumeName, observed)
	}

	return placements, nil
}

// findDiskByBackingPath returns the VirtualDisk device whose backing file
// matches diskPath, or nil if none is present. Checks every flat-file
// backing type, since a snapshotted disk's backing may be a redo log.
func findDiskByBackingPath(devices object.VirtualDeviceList, diskPath string) *vimtypes.VirtualDisk {
	for _, dev := range devices {
		disk, ok := dev.(*vimtypes.VirtualDisk)
		if !ok {
			continue
		}
		switch b := disk.Backing.(type) {
		case *vimtypes.VirtualDiskFlatVer2BackingInfo:
			if b.FileName == diskPath {
				return disk
			}
		case *vimtypes.VirtualDiskSeSparseBackingInfo:
			if b.FileName == diskPath {
				return disk
			}
		case *vimtypes.VirtualDiskSparseVer2BackingInfo:
			if b.FileName == diskPath {
				return disk
			}
		}
	}
	return nil
}

// buildVolumeDisk builds the VirtualDisk device for one VolumeDiskAddSpec
// and resolves its controller slot. When ControllerBusNumber and UnitNumber
// are both set the device is pinned to that exact slot; otherwise it is
// assigned the first free slot on a controller of the requested type (SCSI
// when unset).
func buildVolumeDisk(devices object.VirtualDeviceList, d providers.VolumeDiskAddSpec) (*vimtypes.VirtualDisk, error) {
	vimMode, err := diskModeToVim(d.DiskMode)
	if err != nil {
		return nil, err
	}

	backing := &vimtypes.VirtualDiskFlatVer2BackingInfo{
		DiskMode: string(vimMode),
	}
	backing.FileName = d.DiskPath
	if d.SharingMode == vmopv1.VolumeSharingModeMultiWriter {
		backing.Sharing = string(vimtypes.VirtualDiskSharingSharingMultiWriter)
	}

	disk := &vimtypes.VirtualDisk{
		VirtualDevice: vimtypes.VirtualDevice{
			Backing: backing,
		},
	}

	if d.ControllerBusNumber != nil && d.UnitNumber != nil {
		controller, err := findControllerByTypeAndBus(devices, d.ControllerType, *d.ControllerBusNumber)
		if err != nil {
			return nil, err
		}
		unit := *d.UnitNumber
		disk.ControllerKey = controller.GetVirtualController().Key
		disk.UnitNumber = &unit
		if disk.Key == 0 {
			disk.Key = devices.NewKey()
		}
		return disk, nil
	}

	controller, err := devices.FindDiskController(controllerNameFor(d.ControllerType))
	if err != nil {
		return nil, fmt.Errorf("failed to find a %s disk controller: %w", d.ControllerType, err)
	}
	devices.AssignController(disk, controller)

	return disk, nil
}

// controllerNameFor maps a VirtualControllerType to the name
// object.VirtualDeviceList.FindDiskController expects. An empty type maps to
// "", which FindDiskController resolves to SCSI.
func controllerNameFor(t vmopv1.VirtualControllerType) string {
	switch t {
	case vmopv1.VirtualControllerTypeIDE:
		return "ide"
	case vmopv1.VirtualControllerTypeNVME:
		return "nvme"
	case vmopv1.VirtualControllerTypeSATA:
		return "sata"
	case vmopv1.VirtualControllerTypeSCSI:
		return "scsi"
	default:
		return ""
	}
}

// findControllerByTypeAndBus returns the controller device of the given type
// and bus number.
func findControllerByTypeAndBus(
	devices object.VirtualDeviceList,
	controllerType vmopv1.VirtualControllerType,
	busNumber int32) (vimtypes.BaseVirtualController, error) {

	for _, dev := range devices {
		if ct, bn, ok := controllerTypeAndBus(dev); ok && ct == controllerType && bn == busNumber {
			c, ok := dev.(vimtypes.BaseVirtualController)
			if !ok {
				continue
			}
			return c, nil
		}
	}
	return nil, fmt.Errorf("controller not found: type=%s busNumber=%d", controllerType, busNumber)
}

// controllerTypeAndBus reports the VirtualControllerType and bus number of
// dev if it is a disk controller vm-operator supports, and false otherwise.
func controllerTypeAndBus(dev vimtypes.BaseVirtualDevice) (vmopv1.VirtualControllerType, int32, bool) {
	switch c := dev.(type) {
	case vimtypes.BaseVirtualSCSIController:
		return vmopv1.VirtualControllerTypeSCSI, c.GetVirtualSCSIController().BusNumber, true
	case *vimtypes.VirtualAHCIController:
		return vmopv1.VirtualControllerTypeSATA, c.BusNumber, true
	case *vimtypes.VirtualNVMEController:
		return vmopv1.VirtualControllerTypeNVME, c.BusNumber, true
	case *vimtypes.VirtualIDEController:
		return vmopv1.VirtualControllerTypeIDE, c.BusNumber, true
	default:
		return "", 0, false
	}
}

// diskModeToVim maps vmopv1.VolumeDiskMode to the vSphere API's
// VirtualDiskMode string, treating an empty value as Persistent (matching
// the vm.spec default).
func diskModeToVim(dm vmopv1.VolumeDiskMode) (vimtypes.VirtualDiskMode, error) {
	switch dm {
	case vmopv1.VolumeDiskModePersistent, "":
		return vimtypes.VirtualDiskModePersistent, nil
	case vmopv1.VolumeDiskModeIndependentPersistent:
		return vimtypes.VirtualDiskModeIndependent_persistent, nil
	case vmopv1.VolumeDiskModeIndependentNonPersistent:
		return vimtypes.VirtualDiskModeIndependent_nonpersistent, nil
	case vmopv1.VolumeDiskModeNonPersistent:
		return vimtypes.VirtualDiskModeNonpersistent, nil
	default:
		return "", fmt.Errorf("unknown disk mode %q", dm)
	}
}

// placementFromDisk builds the VolumeDiskPlacement for an attached disk,
// resolving its controller's type and bus number from the device list.
func placementFromDisk(
	devices object.VirtualDeviceList,
	volumeName string,
	disk *vimtypes.VirtualDisk) providers.VolumeDiskPlacement {

	placement := providers.VolumeDiskPlacement{
		VolumeName: volumeName,
	}

	if b, ok := disk.Backing.(*vimtypes.VirtualDiskFlatVer2BackingInfo); ok {
		placement.DiskUUID = b.Uuid
	}

	if disk.UnitNumber != nil {
		placement.UnitNumber = *disk.UnitNumber
	}

	for _, dev := range devices {
		bd := dev.GetVirtualDevice()
		if bd == nil || bd.Key != disk.ControllerKey {
			continue
		}
		if ct, bn, ok := controllerTypeAndBus(dev); ok {
			placement.ControllerType = ct
			placement.ControllerBusNumber = bn
		}
		break
	}

	return placement
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
