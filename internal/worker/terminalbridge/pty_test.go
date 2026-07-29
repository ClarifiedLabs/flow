package terminalbridge

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestOpenPTYReturnsWorkingDescriptors(t *testing.T) {
	master, slave, err := openPTY()
	if err != nil {
		t.Fatalf("openPTY: %v", err)
	}
	defer master.Close()
	defer slave.Close()

	if _, err := slave.Write([]byte("slave-output\n")); err != nil {
		t.Fatalf("write slave: %v", err)
	}
	buffer := make([]byte, 128)
	n, err := readWithTimeout(t, master.Read, buffer, 2*time.Second)
	if err != nil {
		t.Fatalf("read master: %v", err)
	}
	if !bytes.Contains(buffer[:n], []byte("slave-output")) {
		t.Fatalf("master output = %q, want slave output", buffer[:n])
	}

	if _, err := master.Write([]byte("master-input\n")); err != nil {
		t.Fatalf("write master: %v", err)
	}
	n, err = readWithTimeout(t, slave.Read, buffer, 2*time.Second)
	if err != nil {
		t.Fatalf("read slave: %v", err)
	}
	if !bytes.Contains(buffer[:n], []byte("master-input")) {
		t.Fatalf("slave input = %q, want master input", buffer[:n])
	}
}

func readWithTimeout(t *testing.T, read func([]byte) (int, error), buffer []byte, timeout time.Duration) (int, error) {
	t.Helper()
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		n, err := read(buffer)
		ch <- result{n, err}
	}()
	select {
	case res := <-ch:
		return res.n, res.err
	case <-time.After(timeout):
		return 0, fmt.Errorf("read timed out after %v", timeout)
	}
}

func TestSetWinsizeRoundTrip(t *testing.T) {
	master, slave, err := openPTY()
	if err != nil {
		t.Fatalf("openPTY: %v", err)
	}
	defer master.Close()
	defer slave.Close()

	if err := SetWinsize(master, 43, 117); err != nil {
		t.Fatalf("SetWinsize: %v", err)
	}
	got, err := unix.IoctlGetWinsize(int(master.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		t.Fatalf("IoctlGetWinsize: %v", err)
	}
	if got.Row != 43 || got.Col != 117 {
		t.Fatalf("winsize = %dx%d, want 43x117", got.Row, got.Col)
	}
}

func TestStartWithSizeReportsRequestedSize(t *testing.T) {
	cmd := exec.Command("sh", "-c", "stty size")
	master, err := StartWithSize(cmd, 31, 97)
	if err != nil {
		t.Fatalf("StartWithSize: %v", err)
	}
	defer master.Close()

	output := readPTYUntil(t, master, []byte("31 97"))
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait: %v (output %q)", err, output)
	}
}

func TestStartWithSizeIORoundTrip(t *testing.T) {
	cmd := exec.Command("cat")
	master, err := StartWithSize(cmd, 24, 80)
	if err != nil {
		t.Fatalf("StartWithSize: %v", err)
	}
	defer master.Close()

	message := []byte("pty-round-trip\n")
	if _, err := master.Write(message); err != nil {
		t.Fatalf("write master: %v", err)
	}
	readPTYUntil(t, master, []byte("pty-round-trip"))

	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatalf("terminate command: %v", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("wait unexpectedly succeeded after SIGTERM")
	}
}

func readPTYUntil(t *testing.T, master *os.File, wanted []byte) []byte {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)

	var output bytes.Buffer
	buffer := make([]byte, 512)
	for {
		if time.Now().After(deadline) {
			t.Fatal(fmt.Sprintf("timed out waiting for %q (output %q)", wanted, output.Bytes()))
		}

		type result struct {
			n   int
			err error
		}
		ch := make(chan result, 1)
		go func() {
			n, err := master.Read(buffer)
			ch <- result{n, err}
		}()
		select {
		case res := <-ch:
			if res.n > 0 {
				output.Write(buffer[:res.n])
				if bytes.Contains(output.Bytes(), wanted) {
					return output.Bytes()
				}
			}
			if res.err != nil {
				if errors.Is(res.err, syscall.EIO) {
					t.Fatalf("PTY closed with output %q before %q", output.Bytes(), wanted)
				}
				t.Fatalf("read PTY waiting for %q: %v (output %q)", wanted, res.err, output.Bytes())
			}
		case <-time.After(time.Until(deadline)):
			t.Fatal(fmt.Sprintf("timed out waiting for %q (output %q)", wanted, output.Bytes()))
		}
	}
}
