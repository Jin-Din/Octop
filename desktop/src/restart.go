package main

import (
	"log"
	"os"
	"os/exec"
)

// relaunch spawns a detached copy of the current executable with the same
// arguments and working directory, then returns so the caller can tear the
// running instance down. Detachment keeps the new process alive after this
// process exits and, on Windows, outside the portable child process group so
// taskkill /T on the old tree cannot reap it.
func relaunch() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Dir, err = os.Getwd()
	if err != nil {
		return err
	}
	detachProcess(cmd)
	return cmd.Start()
}

// RestartApp stops any locally managed portable server, starts a fresh copy of
// the shell, and quits the current instance. Remote-only shells have no child
// process to stop.
func (a *App) RestartApp() error {
	a.mu.Lock()
	cmd := a.cmd
	a.cmd = nil
	ownsPort := a.ownsListenPort
	a.ownsListenPort = false
	a.mu.Unlock()

	stopOctop(cmd)
	if ownsPort {
		clearDesktopPort()
	}
	if err := relaunch(); err != nil {
		return err
	}
	log.Printf("restarting Octop desktop")
	a.requestQuit()
	return nil
}
