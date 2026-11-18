//go:build !linux

package apparmor

import "errors"

// probeRuncLabel has no kernel to ask outside Linux.
func probeRuncLabel() (string, error) {
	return "", errors.New("AppArmor labels are Linux-only")
}
