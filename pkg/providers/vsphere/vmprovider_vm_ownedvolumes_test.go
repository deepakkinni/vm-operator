// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package vsphere

import (
	"testing"

	"github.com/vmware/govmomi/object"
	vimtypes "github.com/vmware/govmomi/vim25/types"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha6"
	"github.com/vmware-tanzu/vm-operator/pkg/providers"
)

func TestNormalizeBackingFileName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "https folder URL is converted to datastore path",
			input:    "https://lvn-dvm-10-163-8-44.dvm.lvn.broadcom.net:443/folder/d1105dd4-64fc-4f10-85e4-b1459634cf16/vm-test-2_4.vmdk?dcPath=%2Ftest-vpx-1781551215-445895-wcp.wcp-sanity&dsName=sharedVmfs-0",
			expected: "[sharedVmfs-0] d1105dd4-64fc-4f10-85e4-b1459634cf16/vm-test-2_4.vmdk",
		},
		{
			name:     "http folder URL is converted to datastore path",
			input:    "http://vc.example.com/folder/my-vm/disk.vmdk?dcPath=%2Fdc1&dsName=LocalDS_0",
			expected: "[LocalDS_0] my-vm/disk.vmdk",
		},
		{
			name:     "already a datastore path is returned unchanged",
			input:    "[sharedVmfs-0] d1105dd4-64fc-4f10-85e4-b1459634cf16/vm-test-2_4.vmdk",
			expected: "[sharedVmfs-0] d1105dd4-64fc-4f10-85e4-b1459634cf16/vm-test-2_4.vmdk",
		},
		{
			name:     "empty string is returned unchanged",
			input:    "",
			expected: "",
		},
		{
			name:     "https URL without /folder/ prefix is returned unchanged",
			input:    "https://vc.example.com/other/path/disk.vmdk?dsName=ds1",
			expected: "https://vc.example.com/other/path/disk.vmdk?dsName=ds1",
		},
		{
			name:     "https folder URL without dsName is returned unchanged",
			input:    "https://vc.example.com/folder/my-vm/disk.vmdk?dcPath=%2Fdc1",
			expected: "https://vc.example.com/folder/my-vm/disk.vmdk?dcPath=%2Fdc1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeBackingFileName(tc.input)
			if got != tc.expected {
				t.Errorf("normalizeBackingFileName(%q)\n  got:  %q\n  want: %q", tc.input, got, tc.expected)
			}
		})
	}
}

func TestDiskModeToVim(t *testing.T) {
	tests := []struct {
		name     string
		mode     vmopv1.VolumeDiskMode
		expected vimtypes.VirtualDiskMode
		wantErr  bool
	}{
		{name: "empty defaults to persistent", mode: "", expected: vimtypes.VirtualDiskModePersistent},
		{name: "persistent", mode: vmopv1.VolumeDiskModePersistent, expected: vimtypes.VirtualDiskModePersistent},
		{name: "independent persistent", mode: vmopv1.VolumeDiskModeIndependentPersistent, expected: vimtypes.VirtualDiskModeIndependent_persistent},
		{name: "independent non-persistent", mode: vmopv1.VolumeDiskModeIndependentNonPersistent, expected: vimtypes.VirtualDiskModeIndependent_nonpersistent},
		{name: "non-persistent", mode: vmopv1.VolumeDiskModeNonPersistent, expected: vimtypes.VirtualDiskModeNonpersistent},
		{name: "unknown mode errors", mode: "bogus", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := diskModeToVim(tc.mode)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("diskModeToVim(%q): expected an error, got none", tc.mode)
				}
				return
			}
			if err != nil {
				t.Fatalf("diskModeToVim(%q): unexpected error: %v", tc.mode, err)
			}
			if got != tc.expected {
				t.Errorf("diskModeToVim(%q) = %q, want %q", tc.mode, got, tc.expected)
			}
		})
	}
}

func newSCSIController(key int32, busNumber int32) *vimtypes.VirtualLsiLogicController {
	return &vimtypes.VirtualLsiLogicController{
		VirtualSCSIController: vimtypes.VirtualSCSIController{
			VirtualController: vimtypes.VirtualController{
				VirtualDevice: vimtypes.VirtualDevice{Key: key},
				BusNumber:     busNumber,
			},
		},
	}
}

func newFlatDisk(key, controllerKey, unitNumber int32, fileName string) *vimtypes.VirtualDisk {
	unit := unitNumber
	return &vimtypes.VirtualDisk{
		VirtualDevice: vimtypes.VirtualDevice{
			Key:           key,
			ControllerKey: controllerKey,
			UnitNumber:    &unit,
			Backing:       &vimtypes.VirtualDiskFlatVer2BackingInfo{VirtualDeviceFileBackingInfo: vimtypes.VirtualDeviceFileBackingInfo{FileName: fileName}},
		},
	}
}

func TestFindDiskByBackingPath(t *testing.T) {
	devices := object.VirtualDeviceList{
		newSCSIController(1000, 0),
		newFlatDisk(2000, 1000, 0, "[ds1] disk-a.vmdk"),
	}

	if got := findDiskByBackingPath(devices, "[ds1] disk-a.vmdk"); got == nil {
		t.Fatal("expected to find disk-a.vmdk, got nil")
	}
	if got := findDiskByBackingPath(devices, "[ds1] disk-missing.vmdk"); got != nil {
		t.Fatalf("expected no match for a path not on the VM, got %+v", got)
	}
}

func TestBuildVolumeDisk(t *testing.T) {
	t.Run("implicit slot picks a free unit on the first SCSI controller", func(t *testing.T) {
		devices := object.VirtualDeviceList{
			newSCSIController(1000, 0),
			newFlatDisk(2000, 1000, 0, "[ds1] existing.vmdk"),
		}

		disk, err := buildVolumeDisk(devices, providers.VolumeDiskAddSpec{
			VolumeName: "vol-1",
			DiskPath:   "[ds1] new.vmdk",
			DiskMode:   vmopv1.VolumeDiskModePersistent,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if disk.ControllerKey != 1000 {
			t.Errorf("ControllerKey = %d, want 1000", disk.ControllerKey)
		}
		if disk.UnitNumber == nil || *disk.UnitNumber != 1 {
			t.Errorf("UnitNumber = %v, want 1 (unit 0 is taken)", disk.UnitNumber)
		}
		backing, ok := disk.Backing.(*vimtypes.VirtualDiskFlatVer2BackingInfo)
		if !ok {
			t.Fatalf("Backing is %T, want *VirtualDiskFlatVer2BackingInfo", disk.Backing)
		}
		if backing.DiskMode != string(vimtypes.VirtualDiskModePersistent) {
			t.Errorf("DiskMode = %q, want %q", backing.DiskMode, vimtypes.VirtualDiskModePersistent)
		}
		if backing.Sharing != "" {
			t.Errorf("Sharing = %q, want empty for a non-MultiWriter volume", backing.Sharing)
		}
	})

	t.Run("MultiWriter sharing mode sets the sharing flag", func(t *testing.T) {
		devices := object.VirtualDeviceList{newSCSIController(1000, 0)}

		disk, err := buildVolumeDisk(devices, providers.VolumeDiskAddSpec{
			VolumeName:  "vol-1",
			DiskPath:    "[ds1] new.vmdk",
			DiskMode:    vmopv1.VolumeDiskModeIndependentPersistent,
			SharingMode: vmopv1.VolumeSharingModeMultiWriter,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		backing := disk.Backing.(*vimtypes.VirtualDiskFlatVer2BackingInfo)
		if backing.Sharing != string(vimtypes.VirtualDiskSharingSharingMultiWriter) {
			t.Errorf("Sharing = %q, want %q", backing.Sharing, vimtypes.VirtualDiskSharingSharingMultiWriter)
		}
	})

	t.Run("explicit slot pins the exact controller and unit", func(t *testing.T) {
		devices := object.VirtualDeviceList{
			newSCSIController(1000, 0),
			newSCSIController(1001, 1),
		}

		bus := int32(1)
		unit := int32(5)
		disk, err := buildVolumeDisk(devices, providers.VolumeDiskAddSpec{
			VolumeName:          "vol-1",
			DiskPath:            "[ds1] new.vmdk",
			DiskMode:            vmopv1.VolumeDiskModePersistent,
			ControllerType:      vmopv1.VirtualControllerTypeSCSI,
			ControllerBusNumber: &bus,
			UnitNumber:          &unit,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if disk.ControllerKey != 1001 {
			t.Errorf("ControllerKey = %d, want 1001 (bus 1's controller)", disk.ControllerKey)
		}
		if disk.UnitNumber == nil || *disk.UnitNumber != 5 {
			t.Errorf("UnitNumber = %v, want 5", disk.UnitNumber)
		}
	})

	t.Run("explicit slot on a non-existent bus errors", func(t *testing.T) {
		devices := object.VirtualDeviceList{newSCSIController(1000, 0)}

		bus := int32(7)
		unit := int32(0)
		_, err := buildVolumeDisk(devices, providers.VolumeDiskAddSpec{
			VolumeName:          "vol-1",
			DiskPath:            "[ds1] new.vmdk",
			DiskMode:            vmopv1.VolumeDiskModePersistent,
			ControllerType:      vmopv1.VirtualControllerTypeSCSI,
			ControllerBusNumber: &bus,
			UnitNumber:          &unit,
		})
		if err == nil {
			t.Fatal("expected an error for a non-existent controller bus, got none")
		}
	})

	t.Run("FcdID sets VDiskId on the device", func(t *testing.T) {
		devices := object.VirtualDeviceList{newSCSIController(1000, 0)}

		disk, err := buildVolumeDisk(devices, providers.VolumeDiskAddSpec{
			VolumeName: "vol-1",
			DiskPath:   "[ds1] retained.vmdk",
			DiskMode:   vmopv1.VolumeDiskModePersistent,
			FcdID:      "fcd-volume-id-123",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if disk.VDiskId == nil || disk.VDiskId.Id != "fcd-volume-id-123" {
			t.Errorf("VDiskId = %+v, want Id=fcd-volume-id-123", disk.VDiskId)
		}
	})

	t.Run("no FcdID leaves VDiskId unset", func(t *testing.T) {
		devices := object.VirtualDeviceList{newSCSIController(1000, 0)}

		disk, err := buildVolumeDisk(devices, providers.VolumeDiskAddSpec{
			VolumeName: "vol-1",
			DiskPath:   "[ds1] plain.vmdk",
			DiskMode:   vmopv1.VolumeDiskModePersistent,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if disk.VDiskId != nil {
			t.Errorf("VDiskId = %+v, want nil for a disk that is not a registered FCD", disk.VDiskId)
		}
	})
}

func TestAssertNoVMLevelCBT(t *testing.T) {
	t.Run("no ChangeTrackingEnabled is allowed", func(t *testing.T) {
		if err := assertNoVMLevelCBT(&vimtypes.VirtualMachineConfigSpec{}); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("ChangeTrackingEnabled set, either value, is rejected", func(t *testing.T) {
		for _, v := range []bool{true, false} {
			enabled := v
			err := assertNoVMLevelCBT(&vimtypes.VirtualMachineConfigSpec{ChangeTrackingEnabled: &enabled})
			if err == nil {
				t.Errorf("ChangeTrackingEnabled=%v: expected an error, got none", v)
			}
		}
	})
}

func TestPlacementFromDisk(t *testing.T) {
	devices := object.VirtualDeviceList{
		newSCSIController(1000, 0),
	}
	disk := newFlatDisk(2000, 1000, 3, "[ds1] disk-a.vmdk")
	disk.Backing.(*vimtypes.VirtualDiskFlatVer2BackingInfo).Uuid = "uuid-abc"
	devices = append(devices, disk)

	p := placementFromDisk(devices, "vol-1", disk)

	if p.VolumeName != "vol-1" {
		t.Errorf("VolumeName = %q, want vol-1", p.VolumeName)
	}
	if p.DiskUUID != "uuid-abc" {
		t.Errorf("DiskUUID = %q, want uuid-abc", p.DiskUUID)
	}
	if p.ControllerType != vmopv1.VirtualControllerTypeSCSI {
		t.Errorf("ControllerType = %q, want SCSI", p.ControllerType)
	}
	if p.ControllerBusNumber != 0 {
		t.Errorf("ControllerBusNumber = %d, want 0", p.ControllerBusNumber)
	}
	if p.UnitNumber != 3 {
		t.Errorf("UnitNumber = %d, want 3", p.UnitNumber)
	}
}

func TestRootBackingFileName(t *testing.T) {
	mkBacking := func(fileName string, parent *vimtypes.VirtualDiskFlatVer2BackingInfo) *vimtypes.VirtualDiskFlatVer2BackingInfo {
		return &vimtypes.VirtualDiskFlatVer2BackingInfo{
			VirtualDeviceFileBackingInfo: vimtypes.VirtualDeviceFileBackingInfo{FileName: fileName},
			Parent:                       parent,
		}
	}

	tests := []struct {
		name     string
		backing  *vimtypes.VirtualDiskFlatVer2BackingInfo
		expected string
	}{
		{
			name:     "no parent — base disk returned as-is",
			backing:  mkBacking("[ds] vm/disk.vmdk", nil),
			expected: "[ds] vm/disk.vmdk",
		},
		{
			name: "one level of delta — parent returned",
			backing: mkBacking(
				"[ds] vm/disk-000001.vmdk",
				mkBacking("[ds] vm/disk.vmdk", nil),
			),
			expected: "[ds] vm/disk.vmdk",
		},
		{
			name: "two levels of delta — grandparent returned",
			backing: mkBacking(
				"[ds] vm/disk-000002.vmdk",
				mkBacking(
					"[ds] vm/disk-000001.vmdk",
					mkBacking("[ds] vm/disk.vmdk", nil),
				),
			),
			expected: "[ds] vm/disk.vmdk",
		},
		{
			name: "https URL parent is normalised",
			backing: mkBacking(
				"[ds] vm/disk-000001.vmdk",
				mkBacking("https://vc.example.com/folder/vm/disk.vmdk?dcPath=%2Fdc1&dsName=ds", nil),
			),
			expected: "[ds] vm/disk.vmdk",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := rootBackingFileName(tc.backing)
			if got != tc.expected {
				t.Errorf("rootBackingFileName()\n  got:  %q\n  want: %q", got, tc.expected)
			}
		})
	}
}
