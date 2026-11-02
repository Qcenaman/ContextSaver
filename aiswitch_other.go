//go:build !windows

package main

import "syscall"

func askChatClose(pid int) { syscall.Kill(pid, syscall.SIGTERM) }

func writeModelIcons(exe string, names []string) {}
