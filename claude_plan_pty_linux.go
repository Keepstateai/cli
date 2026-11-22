//go:build linux

package main

import (
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// ptyPair opens a new pseudo-terminal: the master this process reads and
// writes, and the slave the child runs on.
func ptyPair() (master, slave *os.File, err error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	var n uint32
	if err := ioctlOn(m, func(fd int) error {
		if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
			return err
		}
		var gerr error
		n, gerr = unix.IoctlGetUint32(fd, unix.TIOCGPTN)
		return gerr
	}); err != nil {
		m.Close()
		return nil, nil, err
	}
	s, err := os.OpenFile("/dev/pts/"+strconv.FormatUint(uint64(n), 10), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		return nil, nil, err
	}
	return m, s, nil
}
