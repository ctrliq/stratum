// SPDX-License-Identifier: MPL-2.0

//go:build linux

// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package pathrs

import (
	"github.com/cyphar/filepath-securejoin/pathrs-lite/internal/fd"
	"github.com/cyphar/filepath-securejoin/pathrs-lite/internal/gocompat"
)

// OpenInRoot stands in for the real function.
func OpenInRoot(root, unsafePath string) (fd.Fd, error) {
	return fd.Fd(gocompat.Zero), nil
}
