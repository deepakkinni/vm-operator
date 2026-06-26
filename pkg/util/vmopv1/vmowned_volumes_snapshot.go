// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package vmopv1

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha6"
	backupapi "github.com/vmware-tanzu/vm-operator/pkg/backup/api"
)

// SnapshotRetentionProvider is the subset of the VM provider interface required
// to evaluate whether a vm-owned-volume disk is still retained by a VM
// snapshot. The full providers.VirtualMachineProviderInterface satisfies it.
type SnapshotRetentionProvider interface {
	// GetPVCDiskDataFromSnapshot decodes the PVC-backed disk list recorded in
	// the named snapshot's frozen ExtraConfig.
	GetPVCDiskDataFromSnapshot(ctx context.Context, vm *vmopv1.VirtualMachine, snapshotName string) ([]backupapi.PVCDiskData, error)

	// IsDiskRetainedByAnySnapshot walks the VM's live vCenter snapshot tree and
	// reports whether any snapshot — including unmanaged snapshots with no CR —
	// retains a virtual disk with the given backing UUID.
	IsDiskRetainedByAnySnapshot(ctx context.Context, vm *vmopv1.VirtualMachine, diskUUID string) (bool, error)
}

// IsDiskRetainedBySnapshot reports whether any VM snapshot still retains the
// disk backing the given PVC, using the two-tier check mandated by the
// VM-owned-volume spec (§4.2.3, §5.4, §10.2 D.4, §11.2 E.5):
//
//  1. Fast path (managed): scan every VirtualMachineSnapshot CR for this VM
//     (other than excludeSnapshotName) and check whether its decoded
//     pvc.disk.data references pvcName.
//
//  2. Authoritative backstop (mandatory): query the live vCenter snapshot tree
//     for a VirtualDisk whose backing UUID matches diskUUID. This tier covers
//     unmanaged snapshots (no CR) that are invisible to the fast path yet can
//     still pin the VMDK.
//
// excludeSnapshotName is the snapshot to skip in the fast path — set it to the
// snapshot currently being deleted (Workflow D), or to the empty string to
// consider every snapshot (Workflow B/E detach, where no snapshot is being
// removed).
//
// The entry in CsiVolumeInfo.spec.vms acts as a hold that keeps CSI from
// re-registering the volume while any snapshot retains it; callers must keep
// the entry whenever this returns true.
func IsDiskRetainedBySnapshot(
	ctx context.Context,
	c ctrlclient.Client,
	provider SnapshotRetentionProvider,
	logger logr.Logger,
	vm *vmopv1.VirtualMachine,
	excludeSnapshotName, pvcName, diskUUID string) (bool, error) {

	// --- Fast path: managed VirtualMachineSnapshot CRs ---
	var snapshotList vmopv1.VirtualMachineSnapshotList
	if err := c.List(ctx, &snapshotList,
		ctrlclient.InNamespace(vm.Namespace)); err != nil {
		return false, fmt.Errorf("failed to list VirtualMachineSnapshots: %w", err)
	}

	for i := range snapshotList.Items {
		snap := &snapshotList.Items[i]

		if snap.Name == excludeSnapshotName {
			continue
		}
		if snap.Spec.VMName != vm.Name {
			continue
		}

		snapDisks, err := provider.GetPVCDiskDataFromSnapshot(ctx, vm, snap.Name)
		if err != nil {
			logger.V(4).Info("Failed to read PVC disk data from snapshot, skipping",
				"snapshotName", snap.Name, "error", err.Error())
			continue
		}

		for _, d := range snapDisks {
			if d.PVCName == pvcName {
				return true, nil
			}
		}
	}

	// --- Authoritative backstop: live vCenter snapshot tree ---
	// This covers unmanaged snapshots (no CR) that are invisible above.
	if diskUUID == "" {
		// No UUID available — cannot perform the vCenter check. Treat as not
		// retained to avoid permanently blocking re-registration, but log so
		// the operator can investigate.
		logger.Info("Disk UUID is empty; skipping vCenter snapshot tree check",
			"pvcName", pvcName)
		return false, nil
	}

	retained, err := provider.IsDiskRetainedByAnySnapshot(ctx, vm, diskUUID)
	if err != nil {
		return false, fmt.Errorf("failed to check vCenter snapshot tree for disk %s: %w", diskUUID, err)
	}

	return retained, nil
}
