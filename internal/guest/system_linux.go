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
	"slices"
	"strconv"
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

// fifreeze is FIFREEZE, _IOWR('X', 119, int); x/sys/unix doesn't define it.
const fifreeze = 0xc0045877

func (s *LinuxSystem) Quiesce(ctx context.Context, dirs []string) error {
	under := func(p string) bool {
		for _, d := range dirs {
			if underDir(p, d) {
				return true
			}
		}
		return false
	}

	// Stop everything the job left running (dockerd, background builds):
	// anything chrooted into, or working in, the job's tree. Keep killing
	// until none are left, so a process forked mid-scan is caught too.
	for killUnder(under) > 0 {
		if !sleepCtx(ctx, 20*time.Millisecond) {
			break // whatever is still alive is handled by freezing below
		}
	}

	mounts, err := readMountinfo()
	if err != nil {
		return err
	}
	var stuck []mountEntry
	for i := len(mounts) - 1; i >= 0; i-- {
		m := mounts[i]
		if !under(m.target) {
			continue
		}
		for {
			err := unix.Unmount(m.target, 0)
			if err == nil || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) {
				break // unmounted, or already gone with its parent
			}
			if !errors.Is(err, unix.EBUSY) || !sleepCtx(ctx, 20*time.Millisecond) {
				stuck = append(stuck, m)
				break
			}
		}
	}
	unix.Sync()

	// A disk that is still mounted is frozen instead: freezing flushes it
	// and blocks further writes, so the snapshot is consistent. Virtual
	// filesystems (proc, binds of /dev, the overlay over the job disk) hold
	// no data of their own.
	var errs []error
	for _, m := range stuck {
		if !strings.HasPrefix(m.source, "/dev/") {
			continue
		}
		if err := freeze(m.target); err != nil {
			errs = append(errs, fmt.Errorf("%s on %s is busy and could not be frozen: %w", m.source, m.target, err))
		}
	}
	return errors.Join(errs...)
}

func underDir(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// killUnder SIGKILLs every process whose root or working directory matches
// and returns how many it signalled.
func killUnder(under func(string) bool) int {
	self := os.Getpid()
	entries, _ := os.ReadDir("/proc")
	n := 0
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == 1 || pid == self {
			continue
		}
		// Zombies fail here and kernel threads resolve to "/"; neither
		// holds a job file open.
		root, _ := os.Readlink(fmt.Sprintf("/proc/%d/root", pid))
		cwd, _ := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
		if (root != "" && under(root)) || (cwd != "" && under(cwd)) {
			if unix.Kill(pid, unix.SIGKILL) == nil {
				n++
			}
		}
	}
	return n
}

type mountEntry struct{ source, target string }

// readMountinfo returns this namespace's mounts in mount order.
func readMountinfo() ([]mountEntry, error) {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	return parseMountinfo(string(b)), nil
}

func parseMountinfo(s string) []mountEntry {
	var out []mountEntry
	for _, line := range strings.Split(s, "\n") {
		// id parent major:minor root target opts [optional...] - fstype source superopts
		f := strings.Fields(line)
		sep := slices.Index(f, "-")
		if len(f) < 5 || sep < 5 || sep+2 >= len(f) {
			continue
		}
		out = append(out, mountEntry{source: unescapeMount(f[sep+2]), target: unescapeMount(f[4])})
	}
	return out
}

// unescapeMount undoes mountinfo's octal escapes (\040 for a space).
func unescapeMount(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func freeze(mountpoint string) error {
	fd, err := unix.Open(mountpoint, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	err = unix.IoctlSetInt(fd, fifreeze, 0)
	if errors.Is(err, unix.EBUSY) {
		return nil // already frozen
	}
	return err
}

// PowerOff flushes disks and powers the VM off. Cloud Hypervisor exits and
// hostd tears the VM down.
func (s *LinuxSystem) PowerOff() error {
	unix.Sync()
	return unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
}

// AcceptVsock listens on the vsock port and returns the first connection.
// hostd connects exactly once per VM. Once the job port is listening it also
// serves readyPort (if non-zero), so hostd's readiness probe before the base
// snapshot can never see an agent that is not yet accepting jobs.
func AcceptVsock(port, readyPort uint32) (io.ReadWriteCloser, error) {
	fd, err := listenVsock(port)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	if readyPort != 0 {
		rfd, err := listenVsock(readyPort)
		if err != nil {
			return nil, err
		}
		go serveReady(rfd)
	}
	return acceptVsock(fd)
}

func listenVsock(port uint32) (int, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("vsock socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("vsock bind %d: %w", port, err)
	}
	if err := unix.Listen(fd, 1); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("vsock listen: %w", err)
	}
	return fd, nil
}

func acceptVsock(fd int) (io.ReadWriteCloser, error) {
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

// serveReady accepts and closes every connection: a successful connect is
// the readiness signal.
func serveReady(fd int) {
	for {
		c, err := acceptVsock(fd)
		if err != nil {
			return
		}
		c.Close()
	}
}
