package main

import (
	"os/exec"
	"syscall"
)

// detachConsole closes the console window Windows opens for a login item.
func detachConsole() {
	_, _, _ = syscall.NewLazyDLL("kernel32.dll").NewProc("FreeConsole").Call()
}

// hideWindow stops a console program, such as gh, opening a window of its own.
func hideWindow(cmd *exec.Cmd) {
	const createNoWindow = 0x08000000
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow, HideWindow: true}
}

// startDetached starts a command with no console window, outliving this process.
func startDetached(name string, args ...string) error {
	const detachedProcess, newProcessGroup = 0x00000008, 0x00000200
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess | newProcessGroup, HideWindow: true}
	return cmd.Start()
}
