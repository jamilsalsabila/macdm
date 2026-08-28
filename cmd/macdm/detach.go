package main

import "syscall"

// detachAttr starts the spawned daemon in its own session so it is not killed
// when this short-lived CLI process exits.
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
