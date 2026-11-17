//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// detachProcess puts the relaunched shell in its own session/process group so
// it keeps running after the current instance exits and is unaffected by the
// SIGTERM sent to the portable child process group.
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
