// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package owned

// VolumeAction is the single action that converges one volume from its
// observed state to its desired state (attach/detach §13.5.1). There is
// deliberately no "Detaching" or other in-flight marker: state is inferred
// fresh from live disk presence, CVI entries, vm.spec.volumes, and the
// snapshot tree on every reconcile (level-triggered, per
// operator-best-practices.md).
type VolumeAction int

const (
	// None: already converged, nothing to do.
	None VolumeAction = iota
	// AppendEntry: the volume is in vm.spec.volumes but has no CVI entry —
	// write one.
	AppendEntry
	// WaitForGreen: a dependent volume's entry is present but CSI has not
	// yet signalled green — requeue.
	WaitForGreen
	// AttachDisk: the volume is ready — issue the device add.
	AttachDisk
	// DetachDisk: the disk is on the VM but the volume has left
	// vm.spec.volumes — issue the device remove.
	DetachDisk
	// RemoveEntry: the disk is not on the VM, the volume is not in
	// vm.spec.volumes, and no snapshot retains it — remove the CVI entry so
	// CSI can re-register.
	RemoveEntry
	// AlertInconsistent: an observation the table does not expect. Never
	// silently repaired — surfaced as an error/event so the invariant
	// violation is visible.
	AlertInconsistent
)

// String renders the action for logs and test failure messages.
func (a VolumeAction) String() string {
	switch a {
	case None:
		return "None"
	case AppendEntry:
		return "AppendEntry"
	case WaitForGreen:
		return "WaitForGreen"
	case AttachDisk:
		return "AttachDisk"
	case DetachDisk:
		return "DetachDisk"
	case RemoveEntry:
		return "RemoveEntry"
	case AlertInconsistent:
		return "AlertInconsistent"
	default:
		return "Unknown"
	}
}

// VolumeObservation is the live state ResolveVolumeAction converges from.
// Every field is a fact read fresh this reconcile — none are cached from a
// prior pass.
type VolumeObservation struct {
	// DiskOnVM is true when the disk device is currently attached to the VM.
	DiskOnVM bool
	// EntryPresent is true when the CsiVolumeInfo has a spec.vms entry for
	// this VM.
	EntryPresent bool
	// InSpecVolumes is true when the volume is currently in
	// vm.spec.volumes.
	InSpecVolumes bool
	// SnapshotRefs is true when a VM snapshot (managed or unmanaged) still
	// retains the disk.
	SnapshotRefs bool
	// GreenSignal is true when CSI has signalled VMManaged/Succeeded with
	// an up-to-date observedGeneration. Only meaningful for a dependent
	// volume; independent volumes are ready as soon as EntryPresent is true
	// (attach/detach §7.3).
	GreenSignal bool
	// Dependent is true for the dependent (ownership-transfer) disk mode.
	// False means independent: the FCD stays registered and CSIManaged.
	Dependent bool
}

// ResolveVolumeAction returns the single action that converges one volume
// from obs to its desired state, encoding attach/detach §13.5.1's decision
// table plus the disk-mode split. Every combination the table marks "should
// not occur" resolves to AlertInconsistent rather than falling through to a
// default action — silently repairing a violated invariant would hide the
// bug that produced it.
func ResolveVolumeAction(obs VolumeObservation) VolumeAction {
	switch {

	// -- Steady states: nothing to do. --
	case obs.InSpecVolumes && obs.DiskOnVM && obs.EntryPresent:
		return None
	case !obs.InSpecVolumes && !obs.DiskOnVM && !obs.EntryPresent:
		return None

	// -- Attach path: volume wants to be on the VM. --
	case obs.InSpecVolumes && !obs.EntryPresent && obs.DiskOnVM:
		// The disk is already attached (e.g. imported) but the CVI does not
		// know it yet. Not a table row the spec anticipates as reachable
		// via the normal attach flow — the entry must exist before a
		// device add is ever issued — so this is an invariant violation to
		// surface, not a state to attach into.
		return AlertInconsistent
	case obs.InSpecVolumes && !obs.EntryPresent && !obs.DiskOnVM:
		return AppendEntry
	case obs.InSpecVolumes && obs.EntryPresent && !obs.DiskOnVM:
		if !obs.Dependent {
			// Independent volumes are ready as soon as the entry exists —
			// there is no green signal to wait for (§7.3). CSI is idle by
			// construction, so gating on GreenSignal here would deadlock
			// every independent volume.
			return AttachDisk
		}
		if obs.GreenSignal {
			return AttachDisk
		}
		return WaitForGreen

	// -- Detach path: volume has left vm.spec.volumes. --
	case !obs.InSpecVolumes && obs.EntryPresent && obs.DiskOnVM:
		return DetachDisk
	case !obs.InSpecVolumes && obs.EntryPresent && !obs.DiskOnVM:
		if obs.SnapshotRefs {
			// A snapshot still pins the disk — the entry is a hold that
			// must persist; removing it now would trigger a premature (and
			// failing) re-registration (§5.4, §11.2 E.5).
			return None
		}
		return RemoveEntry
	case !obs.InSpecVolumes && !obs.EntryPresent && obs.DiskOnVM:
		// The disk is physically attached but neither vm.spec.volumes nor
		// the CVI know about it. VM-owned volumes never attach a disk
		// without first writing the entry, so this combination should not
		// occur on a VM-owned VM.
		return AlertInconsistent

	default:
		return AlertInconsistent
	}
}
