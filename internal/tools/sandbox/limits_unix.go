//go:build unix

package sandbox

import (
	"os/exec"
	"syscall"
)

// applyLimits puts the child in its own process group so a timeout kills the
// whole tree, not just the first process, and caps the resources it may take.
//
// Without the process group, a command that spawns children leaves orphans
// running after the timeout fires — which on a developer machine means a
// runaway build that nothing appears to own.
func applyLimits(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
