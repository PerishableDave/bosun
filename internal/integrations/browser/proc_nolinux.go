//go:build unix && !linux

package browser

import "syscall"

func setDeathSignal(*syscall.SysProcAttr) {}
