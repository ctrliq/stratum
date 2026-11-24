//go:build !linux

package host

// The stratum binary only ships for linux; this exists so tests build on dev machines.
func kernelRelease() string { return "" }
