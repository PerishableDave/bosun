//go:build linux

package browser

import "syscall"

func setDeathSignal(a *syscall.SysProcAttr) { a.Pdeathsig = syscall.SIGKILL }
