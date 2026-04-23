//go:build !windows

package proc

import "os/exec"

func hideConsole(_ *exec.Cmd) {}
