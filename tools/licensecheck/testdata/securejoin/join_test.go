// SPDX-License-Identifier: BSD-3-Clause

package securejoin

import "testing"

func TestSecureJoin(t *testing.T) {
	if _, err := SecureJoin("/", "x"); err != nil {
		t.Fatal(err)
	}
}
