//go:build !failpoint

package failpoint

// Compiled reports whether failpoints are compiled in.
const Compiled = false

// Enabled is always false in release builds.
func Enabled(string) bool { return false }

// Active is always empty in release builds.
func Active() []string { return nil }
