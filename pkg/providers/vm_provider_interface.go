// © Broadcom. All Rights Reserved.
// The term “Broadcom” refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"errors"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vapi/library"
	vimtypes "github.com/vmware/govmomi/vim25/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	imgregv1a1 "github.com/vmware-tanzu/image-registry-operator-api/api/v1alpha1"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha6"
	infrav1 "github.com/vmware-tanzu/vm-operator/external/infra/api/v1alpha1"
	backupapi "github.com/vmware-tanzu/vm-operator/pkg/backup/api"
	"github.com/vmware-tanzu/vm-operator/pkg/util/vsphere/client"
)

var (
	// ErrTooManyCreates is returned from the CreateOrUpdateVirtualMachine and
	// CreateOrUpdateVirtualMachineAsync functions when the number of create
	// threads/goroutines have reached the allowed limit.
	ErrTooManyCreates = errors.New("too many creates")

	// ErrReconcileInProgress is returned from the
	// CreateOrUpdateVirtualMachine and DeleteVirtualMachine functions when
	// the VM is still being reconciled in a background thread.
	ErrReconcileInProgress = errors.New("reconcile already in progress")

	// ErrDiskNotFoundAtSlot is returned from the slot-addressed disk
	// functions — DetachDiskAtSlot, GetLiveDiskPathAtSlot — when the live VM
	// carries no virtual disk at the requested slot, either because no disk
	// occupies the unit number or because the controller itself is absent.
	//
	// This is an expected outcome, not a failure: a snapshot revert removes
	// any disk added after the snapshot was taken, while the status entry
	// naming its slot survives (status.volumes for managed volumes tracks
	// attachment state, not live hardware). Callers must distinguish this
	// case from a genuine vCenter error and treat the disk as already
	// detached.
	ErrDiskNotFoundAtSlot = errors.New("virtual disk not found at slot")
)

type VMGroupPlacement struct {
	VMGroup   *vmopv1.VirtualMachineGroup
	VMMembers []*vmopv1.VirtualMachine
}

// VirtualMachineProviderInterface is a pluggable interface for VM Providers.
type VirtualMachineProviderInterface interface {
	CreateOrUpdateVirtualMachine(ctx context.Context, vm *vmopv1.VirtualMachine) error
	CreateOrUpdateVirtualMachineAsync(ctx context.Context, vm *vmopv1.VirtualMachine) (<-chan error, error)
	DeleteVirtualMachine(ctx context.Context, vm *vmopv1.VirtualMachine) error
	// CleanupVirtualMachine removes all VM Operator modifications from a vCenter VM
	// without deleting it. This is used when a VM has the skip-delete-platform-resource
	// annotation to ensure the vCenter VM is left in a clean state.
	CleanupVirtualMachine(ctx context.Context, vm *vmopv1.VirtualMachine) error
	PublishVirtualMachine(ctx context.Context, vm *vmopv1.VirtualMachine,
		vmPub *vmopv1.VirtualMachinePublishRequest, cl *imgregv1a1.ContentLibrary, actID string) (string, error)
	GetVirtualMachineGuestHeartbeat(ctx context.Context, vm *vmopv1.VirtualMachine) (vmopv1.GuestHeartbeatStatus, error)
	GetVirtualMachineProperties(ctx context.Context, vm *vmopv1.VirtualMachine, propertyPaths []string) (map[string]any, error)
	GetVirtualMachineFiles(ctx context.Context, vm *vmopv1.VirtualMachine) ([]vimtypes.VirtualMachineFileLayoutExFileInfo, error)
	GetVirtualMachineWebMKSTicket(ctx context.Context, vm *vmopv1.VirtualMachine, pubKey string) (string, error)
	GetVirtualMachineHardwareVersion(ctx context.Context, vm *vmopv1.VirtualMachine) (vimtypes.HardwareVersion, error)
	PlaceVirtualMachineGroup(ctx context.Context, group *vmopv1.VirtualMachineGroup, groupPlacements []VMGroupPlacement) error

	CreateOrUpdateVirtualMachineSetResourcePolicy(ctx context.Context, resourcePolicy *vmopv1.VirtualMachineSetResourcePolicy) error
	DeleteVirtualMachineSetResourcePolicy(ctx context.Context, resourcePolicy *vmopv1.VirtualMachineSetResourcePolicy) error

	// "Infra" related
	UpdateVcPNID(ctx context.Context, vcPNID, vcPort string) error
	UpdateVcCreds(ctx context.Context, data map[string][]byte) error
	ComputeCPUMinFrequency(ctx context.Context) error

	GetItemFromLibraryByName(ctx context.Context, contentLibrary, itemName string) (*library.Item, error)
	GetItemFromInventoryByName(ctx context.Context, contentLibrary, itemName string) (object.Reference, error)
	ContainsExtraConfigEntry(ctx context.Context, objVM *object.VirtualMachine, key, value string) (bool, error)

	UpdateContentLibraryItem(ctx context.Context, itemID, newName string, newDescription *string) error
	SyncVirtualMachineImage(ctx context.Context, cli, vmi ctrlclient.Object) error

	GetTasksByActID(ctx context.Context, vm *vmopv1.VirtualMachine, actID string) (tasksInfo []vimtypes.TaskInfo, retErr error)

	// DoesProfileSupportEncryption returns true if the specified profile
	// supports encryption by checking whether or not the underlying policy
	// contains any IOFILTERs.
	DoesProfileSupportEncryption(ctx context.Context, profileID string) (bool, error)

	// GetStoragePolicyStatus returns the status information for a given
	// storage policy.
	GetStoragePolicyStatus(ctx context.Context, profileID string) (infrav1.StoragePolicyStatus, error)

	// VSphereClient returns the provider's vSphere client.
	VSphereClient(context.Context) (*client.Client, error)

	// DeleteSnapshot deletes a snapshot from a virtual machine.
	DeleteSnapshot(ctx context.Context, vmSnapshot *vmopv1.VirtualMachineSnapshot,
		vm *vmopv1.VirtualMachine, removeChildren bool, consolidate *bool) (bool, error)
	// GetSnapshotSize returns the size of a snapshot.
	GetSnapshotSize(ctx context.Context, vmSnapshotName string, vm *vmopv1.VirtualMachine) (int64, error)
	// SyncVMSnapshotTreeStatus syncs the VM's current and root snapshots status.
	SyncVMSnapshotTreeStatus(ctx context.Context, vm *vmopv1.VirtualMachine) error

	// DetachDiskAtSlot removes the virtual disk at the given SCSI/controller
	// slot from the virtual machine without deleting the underlying VMDK file.
	// The slot is identified by controllerType, controllerBusNumber, and
	// unitNumber as recorded in vm.status.volumes. Returns an error wrapping
	// ErrDiskNotFoundAtSlot if the slot holds no disk.
	DetachDiskAtSlot(ctx context.Context, vm *vmopv1.VirtualMachine, controllerType vmopv1.VirtualControllerType, controllerBusNumber, unitNumber int32) (diskPath string, retErr error)

	// AttachVolumeDisks adds each of the given disks to the VM in a single
	// ReconfigVM_Task. A disk already present at its backing path is omitted
	// from the request but still reported in the result, so a partially
	// applied batch converges on retry. Returns the resolved slot of every
	// disk in disks, whether newly added or already present.
	AttachVolumeDisks(ctx context.Context, vm *vmopv1.VirtualMachine, disks []VolumeDiskAddSpec) ([]VolumeDiskPlacement, error)

	// GetPVCDiskDataFromSnapshot reads the PVCDiskData ExtraConfig key from the
	// named vSphere snapshot and returns the decoded list of PVC-backed disk
	// entries. Returns an empty slice (not an error) if the snapshot has no
	// PVCDiskData key or if the VM does not have the VMOwnedVolumes annotation.
	GetPVCDiskDataFromSnapshot(ctx context.Context, vm *vmopv1.VirtualMachine, snapshotName string) ([]backupapi.PVCDiskData, error)

	// GetDiskPathAtSlot returns the datastore path of the virtual disk at the
	// given controller slot without detaching it. Returns an error if no disk
	// is found at that slot.
	GetDiskPathAtSlot(ctx context.Context, vm *vmopv1.VirtualMachine, controllerType vmopv1.VirtualControllerType, controllerBusNumber, unitNumber int32) (string, error)

	// GetLiveDiskPathAtSlot returns the current, non-base-walked datastore
	// path of the virtual disk at the given controller slot, without
	// modifying the VM. Used to refresh CsiVolumeInfo.spec.diskPath before a
	// detach removes the device (attach/detach §8.2 B.2). Returns an error
	// wrapping ErrDiskNotFoundAtSlot if the slot holds no disk.
	GetLiveDiskPathAtSlot(ctx context.Context, vm *vmopv1.VirtualMachine, controllerType vmopv1.VirtualControllerType, controllerBusNumber, unitNumber int32) (string, error)

	// IsDiskRetainedByAnySnapshot queries the live vCenter snapshot tree for the
	// given VM and reports whether any snapshot — including unmanaged snapshots
	// that have no VirtualMachineSnapshot CR — retains a virtual disk with the
	// given backing UUID. This is the authoritative retention check; it must be
	// used as the final backstop after the fast-path managed-snapshot check.
	IsDiskRetainedByAnySnapshot(ctx context.Context, vm *vmopv1.VirtualMachine, diskUUID string) (bool, error)

	// GetDiskPathFromSnapshot returns the base VMDK datastore path for the
	// disk with the given UUID from the named vSphere snapshot's device config.
	// The path is resolved to the root ancestor (past any redo-log delta
	// suffixes) so it is directly usable for CNS registerDisk. Must be called
	// BEFORE DeleteSnapshot while the snapshot config is still accessible.
	GetDiskPathFromSnapshot(ctx context.Context, vm *vmopv1.VirtualMachine, snapshotName, diskUUID string) (string, error)

	// HasAnySnapshot reports whether the VM has any vSphere snapshot,
	// managed or unmanaged. Used as the migration §4.5 precheck before a VKS
	// node's disk-mode conversion, which the host refuses VM-wide when any
	// snapshot is present.
	HasAnySnapshot(ctx context.Context, vm *vmopv1.VirtualMachine) (bool, error)

	// ConvertDiskToIndependentPersistent reconfigures the virtual disk at the
	// given controller slot to VirtualDiskMode independent_persistent. This
	// edits an existing device in place — no add, no vDiskId, no CBT
	// directive (migration §4.5) — and must never be combined with a
	// VM-level changeTrackingEnabled change (attach/detach §5.6).
	ConvertDiskToIndependentPersistent(ctx context.Context, vm *vmopv1.VirtualMachine, controllerType vmopv1.VirtualControllerType, controllerBusNumber, unitNumber int32) error
}
