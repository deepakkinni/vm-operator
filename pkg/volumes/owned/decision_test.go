// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package owned_test

import (
	"testing"

	. "github.com/vmware-tanzu/vm-operator/pkg/volumes/owned"
)

func TestResolveVolumeAction(t *testing.T) {
	tests := []struct {
		name     string
		obs      VolumeObservation
		expected VolumeAction
	}{
		{
			name:     "steady state: attached and known everywhere",
			obs:      VolumeObservation{InSpecVolumes: true, DiskOnVM: true, EntryPresent: true},
			expected: None,
		},
		{
			name:     "steady state: fully gone",
			obs:      VolumeObservation{InSpecVolumes: false, DiskOnVM: false, EntryPresent: false},
			expected: None,
		},
		{
			name:     "in spec, on VM, no entry: should not occur",
			obs:      VolumeObservation{InSpecVolumes: true, DiskOnVM: true, EntryPresent: false},
			expected: AlertInconsistent,
		},
		{
			name:     "in spec, not on VM, no entry: append the entry",
			obs:      VolumeObservation{InSpecVolumes: true, DiskOnVM: false, EntryPresent: false},
			expected: AppendEntry,
		},
		{
			name: "in spec, not on VM, entry present, independent: attach immediately",
			obs: VolumeObservation{
				InSpecVolumes: true, DiskOnVM: false, EntryPresent: true,
				Dependent: false, GreenSignal: false,
			},
			expected: AttachDisk,
		},
		{
			name: "in spec, not on VM, entry present, dependent, green: attach",
			obs: VolumeObservation{
				InSpecVolumes: true, DiskOnVM: false, EntryPresent: true,
				Dependent: true, GreenSignal: true,
			},
			expected: AttachDisk,
		},
		{
			name: "in spec, not on VM, entry present, dependent, not green: wait",
			obs: VolumeObservation{
				InSpecVolumes: true, DiskOnVM: false, EntryPresent: true,
				Dependent: true, GreenSignal: false,
			},
			expected: WaitForGreen,
		},
		{
			name:     "not in spec, entry present, on VM: detach",
			obs:      VolumeObservation{InSpecVolumes: false, DiskOnVM: true, EntryPresent: true},
			expected: DetachDisk,
		},
		{
			name: "not in spec, entry present, not on VM, no snapshot: remove entry",
			obs: VolumeObservation{
				InSpecVolumes: false, DiskOnVM: false, EntryPresent: true,
				SnapshotRefs: false,
			},
			expected: RemoveEntry,
		},
		{
			name: "not in spec, entry present, not on VM, snapshot retains: hold",
			obs: VolumeObservation{
				InSpecVolumes: false, DiskOnVM: false, EntryPresent: true,
				SnapshotRefs: true,
			},
			expected: None,
		},
		{
			name:     "not in spec, on VM, no entry: should not occur",
			obs:      VolumeObservation{InSpecVolumes: false, DiskOnVM: true, EntryPresent: false},
			expected: AlertInconsistent,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveVolumeAction(tc.obs)
			if got != tc.expected {
				t.Errorf("ResolveVolumeAction(%+v) = %s, want %s", tc.obs, got, tc.expected)
			}
		})
	}
}

// TestResolveVolumeActionExhaustive walks every one of the 8 combinations of
// the three boolean dimensions (InSpecVolumes, DiskOnVM, EntryPresent) and
// asserts none of them panics or falls through to an unhandled zero value —
// every combination must be an explicit, deliberate decision.
func TestResolveVolumeActionExhaustive(t *testing.T) {
	for _, inSpec := range []bool{true, false} {
		for _, onVM := range []bool{true, false} {
			for _, entry := range []bool{true, false} {
				obs := VolumeObservation{InSpecVolumes: inSpec, DiskOnVM: onVM, EntryPresent: entry}
				got := ResolveVolumeAction(obs)
				if got < None || got > AlertInconsistent {
					t.Errorf("ResolveVolumeAction(%+v) returned an out-of-range action: %v", obs, got)
				}
			}
		}
	}
}
