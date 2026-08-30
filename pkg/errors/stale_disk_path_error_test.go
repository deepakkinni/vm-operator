// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package errors_test

import (
	"errors"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pkgerr "github.com/vmware-tanzu/vm-operator/pkg/errors"
)

var _ = Describe("StaleDiskPathError", func() {

	It("formats the volume names and path into its message", func() {
		e := pkgerr.StaleDiskPathError{
			VolumeNames: []string{"vol-1"},
			Path:        "[ds1] fcd/gone.vmdk",
			Cause:       errors.New("file not found"),
		}
		Expect(e.Error()).To(ContainSubstring("[ds1] fcd/gone.vmdk"))
		Expect(e.Error()).To(ContainSubstring("vol-1"))
	})

	It("unwraps to the cause", func() {
		cause := errors.New("file not found")
		e := pkgerr.StaleDiskPathError{VolumeNames: []string{"vol-1"}, Cause: cause}
		Expect(errors.Unwrap(e)).To(Equal(cause))
	})

	Describe("AsStaleDiskPathError", func() {
		It("returns true and the error when wrapped", func() {
			inner := pkgerr.StaleDiskPathError{VolumeNames: []string{"vol-1"}, Path: "[ds1] a.vmdk"}
			wrapped := fmt.Errorf("attach failed: %w", inner)

			stale, ok := pkgerr.AsStaleDiskPathError(wrapped)
			Expect(ok).To(BeTrue())
			Expect(stale.VolumeNames).To(Equal([]string{"vol-1"}))
		})

		It("returns false for an unrelated error", func() {
			_, ok := pkgerr.AsStaleDiskPathError(errors.New("some other failure"))
			Expect(ok).To(BeFalse())
		})
	})
})
