package guest

import (
	"context"
	"io"
)

// System is every privileged side effect the agent has: finding and mounting
// disks, running processes, powering off. The Linux implementation is in
// system_linux.go; tests use a fake. Plain file operations (mkdir, write,
// rename, remove) go through the os package on paths under Layout.
type System interface {
	// DiskBySerial waits for the hot-plugged disk with this virtio serial
	// and returns its device node.
	DiskBySerial(ctx context.Context, serial string) (string, error)
	// EnsureFilesystem formats dev as ext4 if it has no filesystem yet (an
	// empty job disk or a new sticky volume).
	EnsureFilesystem(ctx context.Context, dev string) error
	// Mount mounts source on target. fstype "bind" is a recursive bind mount.
	Mount(source, target, fstype, data string) error
	Chown(path string, uid, gid int) error
	// Run runs a command to completion and returns its exit code. A non-nil
	// error means it could not be started or waited for.
	Run(ctx context.Context, cmd Command) (int, error)
	// Quiesce makes everything the job wrote durable. hostd destroys the VM
	// and snapshots its disks (the new prebuild, promoted sticky volumes) as
	// soon as it reads the result, so this runs before the result is sent.
	// It kills the processes the job left running under dirs, unmounts
	// every mount at or under dirs (deepest first) and flushes; a
	// block-backed filesystem that cannot be unmounted is frozen instead.
	// An error means the disks may not be consistent.
	Quiesce(ctx context.Context, dirs []string) error
	// StopProcesses kills the processes the job left running under dirs
	// (dockerd, background builds) until none are left.
	StopProcesses(ctx context.Context, dirs []string)
	PowerOff() error
}

// Command is a process the agent starts inside the job's root.
type Command struct {
	Path string
	Args []string
	Env  []string
	// Dir is the working directory, inside Root.
	Dir string
	// Root is the directory to chroot into ("" = none).
	Root string
	// UID and GID to run as.
	UID, GID int
	// Stdout and Stderr receive the process output (nil = discard).
	Stdout, Stderr io.Writer
}
