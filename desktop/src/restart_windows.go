//go:build windows

package main

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// detachProcess starts the relaunched shell in its own process, detached from
// the current console and outside any child process group, so it survives the
// parent's quit and is not killed when the old portable tree is torn down.
//
// HideWindow must NOT be set: it emits STARTUPINFO with STARTF_USESHOWWINDOW +
// SW_HIDE, and Win32 ignores the relaunched process's very first ShowWindow
// call (forcing SW_HIDE instead), which leaves the whole GUI — including the
// main window — invisible after a restart. DETACHED_PROCESS already gives this
// GUI-subsystem shell no console, so the flag was only hiding its windows.
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS,
	}
}
