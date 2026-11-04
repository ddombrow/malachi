//go:build !linux

package sandbox

// ProtectProcess is Linux-specific. Other platforms keep their native process
// inspection policy; this does not add process-environment protection there.
func ProtectProcess() error { return nil }
