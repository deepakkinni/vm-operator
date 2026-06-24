// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package vsphere

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	vimtypes "github.com/vmware/govmomi/vim25/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha6"
	backupapi "github.com/vmware-tanzu/vm-operator/pkg/backup/api"
	pkgcfg "github.com/vmware-tanzu/vm-operator/pkg/config"
	pkgctx "github.com/vmware-tanzu/vm-operator/pkg/context"
	"github.com/vmware-tanzu/vm-operator/pkg/providers/vsphere/virtualmachine"
	res "github.com/vmware-tanzu/vm-operator/pkg/providers/vsphere/resources"
	pkgutil "github.com/vmware-tanzu/vm-operator/pkg/util"
	vmopv1util "github.com/vmware-tanzu/vm-operator/pkg/util/vmopv1"
)

// GetPVCDiskDataFromSnapshot reads the PVCDiskData ExtraConfig key from the
// named vSphere snapshot and returns the decoded list of PVC-backed disk
// entries. Returns an empty slice (not an error) if the snapshot has no
// PVCDiskData key or if the VM is not greenfield.
func (vs *vSphereVMProvider) GetPVCDiskDataFromSnapshot(
	ctx context.Context,
	vm *vmopv1.VirtualMachine,
	snapshotName string) ([]backupapi.PVCDiskData, error) {

	vmCtx := pkgctx.NewVirtualMachineContext(
		pkgctx.WithVCOpID(ctx, vm, "getPVCDiskDataFromSnapshot"),
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

	// Fetch snapshot tree to find the named snapshot node.
	var moVM mo.VirtualMachine
	if err := vcVM.Properties(vmCtx, vcVM.Reference(), []string{"snapshot"}, &moVM); err != nil {
		return nil, fmt.Errorf("failed to fetch VM snapshot properties: %w", err)
	}

	snapNode, err := virtualmachine.FindSnapshot(moVM, snapshotName)
	if err != nil {
		return nil, fmt.Errorf("failed to find snapshot %q: %w", snapshotName, err)
	}

	// Fetch the snapshot's config to read ExtraConfig.
	var moSnap mo.VirtualMachineSnapshot
	if err := vcVM.Properties(vmCtx, snapNode.Snapshot, []string{"config"}, &moSnap); err != nil {
		return nil, fmt.Errorf("failed to fetch snapshot config for %q: %w", snapshotName, err)
	}

	ecList := object.OptionValueList(moSnap.Config.ExtraConfig)
	raw, _ := ecList.GetString(backupapi.PVCDiskDataExtraConfigKey)
	if raw == "" {
		// No PVC disk data in this snapshot — not a greenfield snapshot.
		return nil, nil
	}

	decoded, err := pkgutil.TryToDecodeBase64Gzip([]byte(raw))
	if err != nil {
		return nil, fmt.Errorf("failed to decode PVC disk data from snapshot %q: %w", snapshotName, err)
	}

	var disks []backupapi.PVCDiskData
	if err := json.Unmarshal([]byte(decoded), &disks); err != nil {
		return nil, fmt.Errorf("failed to unmarshal PVC disk data from snapshot %q: %w", snapshotName, err)
	}

	return disks, nil
}

// GetDiskPathAtSlot returns the datastore path of the virtual disk at the
// given controller slot without detaching it. Returns an error if no disk
// is found at that slot.
func (vs *vSphereVMProvider) GetDiskPathAtSlot(
	ctx context.Context,
	vm *vmopv1.VirtualMachine,
	controllerType vmopv1.VirtualControllerType,
	controllerBusNumber, unitNumber int32) (string, error) {

	vmCtx := pkgctx.NewVirtualMachineContext(
		pkgctx.WithVCOpID(ctx, vm, "getDiskPathAtSlot"),
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

	moVM, err := resVM.GetProperties(vmCtx, []string{"config.hardware.device"})
	if err != nil {
		return "", fmt.Errorf("failed to get VM hardware devices: %w", err)
	}

	_, diskPath, err := findVirtualDiskAtSlot(moVM, controllerType, controllerBusNumber, unitNumber)
	if err != nil {
		return "", fmt.Errorf("failed to find virtual disk at slot: %w", err)
	}

	return diskPath, nil
}

// captureDroppedVolumeDiskPaths records the current VMDK datastore paths for
// volumes that will be dropped by the upcoming snapshot revert. This is a
// best-effort, just-in-time capture: if it fails, Workflow B (reconcile
// greenfield detach) will handle re-registration on the next reconcile pass.
//
// NOTE: This covers managed snapshots only (those with a VirtualMachineSnapshot
// CR). Unmanaged snapshots (out-of-band, no CR) require a vCenter snapshot tree
// query as the authoritative backstop; that is left for a follow-up.
func (vs *vSphereVMProvider) captureDroppedVolumeDiskPaths(
	vmCtx pkgctx.VirtualMachineContext,
	vcVM *object.VirtualMachine,
	snapCR *vmopv1.VirtualMachineSnapshot,
	snapNode *vimtypes.VirtualMachineSnapshotTree) error {

	if !pkgcfg.FromContext(vmCtx).Features.VMOwnedVolumes {
		return nil
	}
	if !vmopv1util.IsGreenfieldVM(vmCtx.VM) {
		return nil
	}

	// Read the target snapshot's PVC disk data to know which PVCs were present
	// at snapshot time.
	snapDisks, err := vs.GetPVCDiskDataFromSnapshot(vmCtx, vmCtx.VM, snapCR.Name)
	if err != nil {
		return fmt.Errorf("failed to read PVC disk data from target snapshot %q: %w", snapCR.Name, err)
	}

	// Build a set of PVC names present in the snapshot.
	snapPVCSet := make(map[string]struct{}, len(snapDisks))
	for _, d := range snapDisks {
		snapPVCSet[d.PVCName] = struct{}{}
	}

	// For each volume currently on the VM that is NOT in the snapshot, capture
	// its disk path and record it in the CVI so the attach path can use it.
	for _, vol := range vmCtx.VM.Spec.Volumes {
		if vol.PersistentVolumeClaim == nil {
			continue
		}
		claimName := vol.PersistentVolumeClaim.ClaimName
		if _, inSnapshot := snapPVCSet[claimName]; inSnapshot {
			continue
		}

		// Volume will be dropped by the revert. Capture its current disk path.
		statusEntry := findVolumeStatusEntry(vmCtx.VM, vol.Name)
		if statusEntry == nil ||
			statusEntry.ControllerBusNumber == nil ||
			statusEntry.UnitNumber == nil {
			vmCtx.Logger.V(4).Info("Skipping disk path capture for volume: missing slot info",
				"volumeName", vol.Name)
			continue
		}

		diskPath, err := vs.GetDiskPathAtSlot(
			vmCtx,
			vmCtx.VM,
			statusEntry.ControllerType,
			*statusEntry.ControllerBusNumber,
			*statusEntry.UnitNumber,
		)
		if err != nil {
			vmCtx.Logger.Error(err, "Failed to capture disk path for dropped volume",
				"volumeName", vol.Name, "pvcName", claimName)
			continue
		}

		cvi, err := vmopv1util.GetCVIForPVC(vmCtx, vs.k8sClient, vmCtx.VM.Namespace, claimName)
		if err != nil {
			vmCtx.Logger.V(4).Info("Skipping disk path patch: CVI not found for PVC",
				"pvcName", claimName, "error", err.Error())
			continue
		}

		patch := ctrlclient.MergeFrom(cvi.DeepCopy())
		cvi.Spec.DiskPath = diskPath
		if err := vs.k8sClient.Patch(vmCtx, cvi, patch); err != nil {
			vmCtx.Logger.Error(err, "Failed to patch CVI disk path for dropped volume",
				"volumeName", vol.Name, "pvcName", claimName, "diskPath", diskPath)
			continue
		}

		vmCtx.Logger.Info("Captured disk path for volume that will be dropped by revert",
			"volumeName", vol.Name, "pvcName", claimName, "diskPath", diskPath)
	}

	return nil
}

// findVolumeStatusEntry returns the VirtualMachineVolumeStatus entry for the
// named volume, or nil if not found.
func findVolumeStatusEntry(
	vm *vmopv1.VirtualMachine,
	volName string) *vmopv1.VirtualMachineVolumeStatus {

	for i := range vm.Status.Volumes {
		if vm.Status.Volumes[i].Name == volName {
			return &vm.Status.Volumes[i]
		}
	}
	return nil
}

// IsDiskRetainedByAnySnapshot queries the live vCenter snapshot tree for the
// given VM and reports whether any snapshot — including unmanaged snapshots
// that have no VirtualMachineSnapshot CR — retains a virtual disk with the
// given backing UUID. This is the authoritative retention check covering
// unmanaged snapshots that are invisible to the managed-snapshot fast path.
func (vs *vSphereVMProvider) IsDiskRetainedByAnySnapshot(
	ctx context.Context,
	vm *vmopv1.VirtualMachine,
	diskUUID string) (bool, error) {

	if diskUUID == "" {
		return false, nil
	}

	vmCtx := pkgctx.NewVirtualMachineContext(
		pkgctx.WithVCOpID(ctx, vm, "isDiskRetainedByAnySnapshot"),
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

	// Fetch the VM's snapshot tree.
	var moVM mo.VirtualMachine
	if err := vcVM.Properties(vmCtx, vcVM.Reference(), []string{"snapshot"}, &moVM); err != nil {
		return false, fmt.Errorf("failed to fetch VM snapshot tree: %w", err)
	}

	if moVM.Snapshot == nil || len(moVM.Snapshot.RootSnapshotList) == 0 {
		return false, nil
	}

	// Walk every snapshot node in the tree and check whether the disk UUID
	// appears in its hardware device list. This covers unmanaged snapshots
	// (created outside vm-operator) that have no VirtualMachineSnapshot CR.
	retained, err := isDiskInSnapshotTree(vmCtx, vcVM, moVM.Snapshot.RootSnapshotList, diskUUID)
	if err != nil {
		return false, fmt.Errorf("failed to walk snapshot tree: %w", err)
	}

	return retained, nil
}

// isDiskInSnapshotTree recursively walks the snapshot tree rooted at nodes
// and returns true if any snapshot retains a VirtualDisk whose backing UUID
// equals diskUUID.
func isDiskInSnapshotTree(
	ctx pkgctx.VirtualMachineContext,
	vcVM *object.VirtualMachine,
	nodes []vimtypes.VirtualMachineSnapshotTree,
	diskUUID string) (bool, error) {

	for i := range nodes {
		node := &nodes[i]

		var moSnap mo.VirtualMachineSnapshot
		if err := vcVM.Properties(ctx, node.Snapshot, []string{"config.hardware.device"}, &moSnap); err != nil {
			// Log and continue: a single unreadable snapshot should not abort the check.
			ctx.Logger.V(4).Info("Failed to fetch snapshot device config, skipping node",
				"snapshot", node.Snapshot.Value, "error", err.Error())
			continue
		}

		for _, dev := range moSnap.Config.Hardware.Device {
			disk, ok := dev.(*vimtypes.VirtualDisk)
			if !ok {
				continue
			}
			backing, ok := disk.Backing.(*vimtypes.VirtualDiskFlatVer2BackingInfo)
			if !ok {
				continue
			}
			if backing.Uuid == diskUUID {
				return true, nil
			}
		}

		// Recurse into child snapshots.
		if len(node.ChildSnapshotList) > 0 {
			found, err := isDiskInSnapshotTree(ctx, vcVM, node.ChildSnapshotList, diskUUID)
			if err != nil {
				return false, err
			}
			if found {
				return true, nil
			}
		}
	}

	return false, nil
}
