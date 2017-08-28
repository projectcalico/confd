// +build solaris

package mlock

import (
	"golang.org/x/sys/unix"
	"syscall"
)

func init() {
	supported = true
}

func lockMemory() error {
	// Mlockall prevents all current and future pages from being swapped out.
	return unix.Mlockall(syscall.MCL_CURRENT | syscall.MCL_FUTURE)
}
