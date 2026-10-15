//go:build unix

package browser

import (
	"os/exec" // allowed for this integration only; see integrations_test.go
	"syscall"
)

// setProcAttrs puts Chromium in its own process group, so that stopping a
// session can kill its helper processes too, and (on Linux) kills it if the
// daemon dies.
func setProcAttrs(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	setDeathSignal(cmd.SysProcAttr)
}

// killGroup kills the process group led by pid.
func killGroup(pid int) {
	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}
