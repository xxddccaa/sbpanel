package main

import (
	"os/exec"
	"syscall"
)

// setParentDeathSignal stops the worker when the supervisor dies, even by SIGKILL,
// so an orphaned worker cannot keep the data directory locked.
func setParentDeathSignal(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}
