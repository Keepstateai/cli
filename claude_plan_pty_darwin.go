//go:build darwin

package main

import (
	"bytes"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ptyPair opens a new pseudo-terminal: the master this process reads and
// writes, and the slave the child runs on.
func ptyPair() (master, slave *os.File, err error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	var name [128]byte
	if err := ioctlOn(m, func(fd int) error {
		if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.TIOCPTYGRANT), 0); e != 0 {
			return e
		}
		if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.TIOCPTYUNLK), 0); e != 0 {
			return e
		}
		if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.TIOCPTYGNAME), uintptr(unsafe.Pointer(&name[0]))); e != 0 {
			return e
		}
		return nil
	}); err != nil {
		m.Close()
		return nil, nil, err
	}
	path := string(name[:bytes.IndexByte(name[:], 0)])
	s, err := os.OpenFile(path, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		return nil, nil, err
	}
	return m, s, nil
}
