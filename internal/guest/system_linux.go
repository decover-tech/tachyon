//go:build linux

package guest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// virtioSerialMax is how many bytes of a serial virtio-blk exposes.
const virtioSerialMax = 20

// LinuxSystem is the real System. It needs root inside the guest.
type LinuxSystem struct {
	// DiskTimeout bounds the wait for a hot-plugged disk to appear.
	DiskTimeout time.Duration
}

// NewSystem returns the System for this platform.
func NewSystem() System { return &LinuxSystem{DiskTimeout: 10 * time.Second} }

func (s *LinuxSystem) DiskBySerial(ctx context.Context, serial string) (string, error) {
	want := serial
	if len(want) > virtioSerialMax {
		want = want[:virtioSerialMax]
	}
	deadline := time.Now().Add(s.DiskTimeout)
	for {
		if dev, ok := findSerial(want); ok {
			return dev, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("no block device with serial %q after %s", serial, s.DiskTimeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// findSerial looks in sysfs rather than /dev/disk/by-id so it doesn't
// depend on udev having run.
func findSerial(serial string) (string, bool) {
	entries, _ := os.ReadDir("/sys/block")
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join("/sys/block", e.Name(), "serial"))
		if err != nil {
			continue
		}
		if strings.TrimRight(string(b), "\x00\n ") == serial {
			return "/dev/" + e.Name(), true
		}
	}
	return "", false
}

func (s *LinuxSystem) EnsureFilesystem(ctx context.Context, dev string) error {
	out, err := exec.CommandContext(ctx, "blkid", "-o", "value", "-s", "TYPE", dev).Output()
	if err == nil && strings.TrimSpace(string(out)) != "" {
		return nil
	}
	// blkid exits 2 when the device has no recognizable filesystem.
	var ee *exec.ExitError
	if err != nil && !(errors.As(err, &ee) && ee.ExitCode() == 2) {
		return fmt.Errorf("blkid %s: %w", dev, err)
	}
	if b, err := exec.CommandContext(ctx, "mkfs.ext4", "-q", "-F", dev).CombinedOutput(); err != nil {
		return fmt.Errorf("mkfs.ext4 %s: %w: %s", dev, err, strings.TrimSpace(string(b)))
	}
	return nil
}

func (s *LinuxSystem) Mount(source, target, fstype, data string) error {
	if fstype == "bind" {
		return unix.Mount(source, target, "", unix.MS_BIND|unix.MS_REC, "")
	}
	return unix.Mount(source, target, fstype, 0, data)
}

func (s *LinuxSystem) Chown(path string, uid, gid int) error {
	return os.Lchown(path, uid, gid)
}

func (s *LinuxSystem) Run(ctx context.Context, c Command) (int, error) {
	cmd := exec.CommandContext(ctx, c.Path, c.Args...)
	// c.Path is inside the chroot; don't let exec resolve it out here.
	cmd.Path, cmd.Err = c.Path, nil
	cmd.Env = c.Env
	cmd.Dir = c.Dir
	cmd.Stdout = orDiscard(c.Stdout)
	cmd.Stderr = orDiscard(c.Stderr)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Chroot:     c.Root,
		Credential: &syscall.Credential{Uid: uint32(c.UID), Gid: uint32(c.GID)},
		Setsid:     true,
		Pdeathsig:  syscall.SIGKILL,
	}
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return 0, err
	}
	return 0, nil
}

func orDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}

// PowerOff flushes disks and powers the VM off. Cloud Hypervisor exits and
// hostd tears the VM down.
func (s *LinuxSystem) PowerOff() error {
	unix.Sync()
	return unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
}

// AcceptVsock listens on the vsock port and returns the first connection.
// hostd connects exactly once per VM.
func AcceptVsock(port uint32) (io.ReadWriteCloser, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		return nil, fmt.Errorf("vsock bind %d: %w", port, err)
	}
	if err := unix.Listen(fd, 1); err != nil {
		return nil, fmt.Errorf("vsock listen: %w", err)
	}
	for {
		nfd, _, err := unix.Accept4(fd, unix.SOCK_CLOEXEC)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("vsock accept: %w", err)
		}
		return os.NewFile(uintptr(nfd), "vsock"), nil
	}
}
