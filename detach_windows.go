//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// detach lets a background process outlive ContextSaver (and LM Studio's job object).
func detach(c *exec.Cmd, breakaway bool) {
	flags := uint32(0x08000000 | 0x00000200) // CREATE_NO_WINDOW | CREATE_NEW_PROCESS_GROUP
	if breakaway {
		flags |= 0x01000000 // CREATE_BREAKAWAY_FROM_JOB
	}
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: flags}
}
