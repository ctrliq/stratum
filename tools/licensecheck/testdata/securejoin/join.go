// SPDX-License-Identifier: BSD-3-Clause

// Copyright (C) 2014-2015 Docker Inc & Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package securejoin

import "github.com/cyphar/filepath-securejoin/internal/consts"

// SecureJoin stands in for the real function.
func SecureJoin(root, unsafePath string) (string, error) {
	return root + consts.Separator + unsafePath, nil
}
