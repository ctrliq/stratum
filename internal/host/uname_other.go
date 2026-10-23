//go:build !linux

package host

// kernelRelease is unknowable off Linux; the floor check skips. The stratum
// binary only ships for linux — this exists so the test suite builds on
// developer machines.
func kernelRelease() string { return "" }
