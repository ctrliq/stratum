//go:build !linux

package comps

import "errors"

func peerPID(string) (int, error) {
	return 0, errors.New("socket peer credentials are only read on Linux")
}
