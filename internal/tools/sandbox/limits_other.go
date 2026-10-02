//go:build !unix

package sandbox

import "os/exec"

// applyLimits is a no-op on platforms without POSIX process groups. The
// context deadline still terminates the direct child; grandchildren are not
// reaped, which is documented in docs/security.md as a known limitation on
// Windows rather than silently assumed away.
func applyLimits(cmd *exec.Cmd) {}
