//go:build darwin

package terminalbridge

import (
	"bytes"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

func openPTY() (master *os.File, slave *os.File, err error) {
	masterFD, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_CLOEXEC, 0)
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

	if err = ioctlPTY(masterFD, unix.TIOCPTYGRANT, 0); err != nil {
		return nil, nil, fmt.Errorf("grant PTY slave: %w", err)
	}
	if err = ioctlPTY(masterFD, unix.TIOCPTYUNLK, 0); err != nil {
		return nil, nil, fmt.Errorf("unlock PTY slave: %w", err)
	}

	var name [128]byte
	if err = ioctlPTY(masterFD, unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))); err != nil {
		return nil, nil, fmt.Errorf("get PTY slave name: %w", err)
	}
	nul := bytes.IndexByte(name[:], 0)
	if nul <= 0 {
		return nil, nil, fmt.Errorf("get PTY slave name: response is not NUL-terminated")
	}
	slavePath := string(name[:nul])

	slaveFD, err := unix.Open(slavePath, unix.O_RDWR|unix.O_CLOEXEC, 0)
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

func ioctlPTY(fd int, request uint, argument uintptr) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(request), argument)
	if errno != 0 {
		return errno
	}
	return nil
}
