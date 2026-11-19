//go:build !windows

package main

import "os/exec"

func detachConsole() {}

func hideWindow(*exec.Cmd) {}

func startDetached(name string, args ...string) error { return exec.Command(name, args...).Start() }
