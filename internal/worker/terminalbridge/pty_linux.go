//go:build linux

package terminalbridge

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func openPTY() (master *os.File, slave *os.File, err error) {
	masterFD, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}
	master = os.NewFile(uintptr(masterFD), "/dev/ptmx")
	if master == nil {
		_ = unix.Close(masterFD)
		return nil, nil, fmt.Errorf("open /dev/ptmx: invalid file descriptor")
	}
	defer func() {
		if err != nil {
			_ = master.Close()
		}
	}()

	if err = unix.IoctlSetPointerInt(masterFD, unix.TIOCSPTLCK, 0); err != nil {
		return nil, nil, fmt.Errorf("unlock /dev/ptmx: %w", err)
	}
	n, err := unix.IoctlGetInt(masterFD, unix.TIOCGPTN)
	if err != nil {
		return nil, nil, fmt.Errorf("get PTY number: %w", err)
	}

	slavePath := fmt.Sprintf("/dev/pts/%d", n)
	slaveFD, err := unix.Open(slavePath, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open PTY slave %q: %w", slavePath, err)
	}
	slave = os.NewFile(uintptr(slaveFD), slavePath)
	if slave == nil {
		_ = unix.Close(slaveFD)
		return nil, nil, fmt.Errorf("open PTY slave %q: invalid file descriptor", slavePath)
	}
	return master, slave, nil
}
