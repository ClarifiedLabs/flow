//go:build linux

package terminalbridge

import (
	"testing"
)

// TestOpenPTYUnlocksSlave is a regression test for a bug where TIOCSPTLCK was
// invoked with the lock value as the ioctl argument instead of a pointer to it.
// That caused unlock /dev/ptmx to fail with EFAULT ("bad address") on Linux,
// breaking the worker terminal bridge and any CI run that exercised it.
func TestOpenPTYUnlocksSlave(t *testing.T) {
	master, slave, err := openPTY()
	if err != nil {
		t.Fatalf("openPTY: %v", err)
	}
	defer master.Close()
	defer slave.Close()

	if master.Fd() < 0 || slave.Fd() < 0 {
		t.Fatalf("openPTY returned invalid descriptors: master=%d slave=%d", master.Fd(), slave.Fd())
	}
}
