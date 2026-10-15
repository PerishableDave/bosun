//go:build !unix

package browser

import "os/exec" // allowed for this integration only; see integrations_test.go

func setProcAttrs(*exec.Cmd) {}

func killGroup(int) {}
