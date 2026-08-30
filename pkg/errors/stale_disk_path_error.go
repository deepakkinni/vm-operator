// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package errors

import (
	"errors"
	"fmt"
)

// StaleDiskPathError indicates a ReconfigVM_Task failed because one or more
// disks' backing file no longer exists at the datastore path last recorded
// on their CsiVolumeInfo — the disk was relocated (e.g. a storage vMotion)
// after CSI resolved spec.diskPath. VolumeNames holds the vm.spec.volumes
// entry names (not CVI names) whose requested DiskPath matched the fault's
// file, so the caller can look up each one's CsiVolumeInfo and request a
// refresh rather than retrying with the same stale path.
type StaleDiskPathError struct {
	// VolumeNames are the vm.spec.volumes[*].name entries affected.
	VolumeNames []string
	// Path is the datastore path the fault named as not found.
	Path string
	// Cause is the underlying ReconfigVM_Task error.
	Cause error
}

func (e StaleDiskPathError) Error() string {
	return fmt.Sprintf("stale disk path %q for volume(s) %v: %v", e.Path, e.VolumeNames, e.Cause)
}

func (e StaleDiskPathError) Unwrap() error {
	return e.Cause
}

// AsStaleDiskPathError returns the StaleDiskPathError wrapped in err, if any.
func AsStaleDiskPathError(err error) (StaleDiskPathError, bool) {
	var stale StaleDiskPathError
	return stale, errors.As(err, &stale)
}
