//go:build !linux

package wrapper

// Non-Linux builds are useful for unit tests and console-side tooling. The
// system-mode capability handoff is only available on Linux.
func RaiseBindCap() error { return nil }

func BindCapXattr() []byte { return nil }
