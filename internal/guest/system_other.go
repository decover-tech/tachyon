//go:build !linux

package guest

import (
	"context"
	"errors"
	"io"
)

var errNotLinux = errors.New("the guest agent runs only on Linux")

type otherSystem struct{}

// NewSystem returns the System for this platform. Off Linux every call
// fails; the package still builds so its logic can be tested anywhere.
func NewSystem() System { return otherSystem{} }

func (otherSystem) DiskBySerial(context.Context, string) (string, error) { return "", errNotLinux }
func (otherSystem) EnsureFilesystem(context.Context, string) error       { return errNotLinux }
func (otherSystem) Mount(string, string, string, string) error           { return errNotLinux }
func (otherSystem) Chown(string, int, int) error                         { return errNotLinux }
func (otherSystem) Run(context.Context, Command) (int, error)            { return 0, errNotLinux }
func (otherSystem) PowerOff() error                                      { return errNotLinux }

// AcceptVsock is Linux-only.
func AcceptVsock(uint32) (io.ReadWriteCloser, error) { return nil, errNotLinux }
