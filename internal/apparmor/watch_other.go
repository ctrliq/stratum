//go:build !linux

package apparmor

import "context"

// WatchPolicy has no kernel to watch outside Linux, so it never sends.
func WatchPolicy(context.Context) <-chan struct{} { return nil }
