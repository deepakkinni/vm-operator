// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package vsphere

import (
	"testing"
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
