package terminalbridge

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// SetWinsize changes the terminal dimensions associated with f.
func SetWinsize(f *os.File, rows, cols uint16) error {
	if f == nil {
		return fmt.Errorf("set terminal size: nil file")
	}
	if err := unix.IoctlSetWinsize(int(f.Fd()), unix.TIOCSWINSZ, &unix.Winsize{
		Row: rows,
		Col: cols,
	}); err != nil {
		return fmt.Errorf("set terminal size to %dx%d: %w", rows, cols, err)
	}
	return nil
}

// StartWithSize starts cmd with a new controlling terminal and returns the
// terminal's master side. The caller owns the returned file.
func StartWithSize(cmd *exec.Cmd, rows, cols uint16) (*os.File, error) {
	if cmd == nil {
		return nil, fmt.Errorf("start command with terminal: nil command")
	}

	master, slave, err := openPTY()
	if err != nil {
		return nil, fmt.Errorf("open terminal: %w", err)
	}
	closeBoth := func() {
		_ = slave.Close()
		_ = master.Close()
	}

	if err := SetWinsize(master, rows, cols); err != nil {
		closeBoth()
		return nil, err
	}

	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    0,
	}
	if err := cmd.Start(); err != nil {
		closeBoth()
		return nil, fmt.Errorf("start command with terminal: %w", err)
	}

	// The child has its own descriptors for the slave. Keeping the parent's
	// descriptor open would prevent EOF on the master when the child exits.
	_ = slave.Close()
	return master, nil
}
