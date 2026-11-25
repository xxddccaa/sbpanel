//go:build !linux

package main

import "os/exec"

func setParentDeathSignal(*exec.Cmd) {}
