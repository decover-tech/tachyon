package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	hostlinkv1 "github.com/decover-tech/tachyon/gen/hostlink/v1"
)

// fakeAuth accepts only id and reports expires.
type fakeAuth struct {
	id      string
	expires time.Time
}

func (a fakeAuth) AuthenticateHost(_ context.Context, hostID string) (time.Time, error) {
	if hostID != a.id {
		return time.Time{}, errors.New("identity mismatch")
	}
	return a.expires, nil
}

func helloStream(id string) (*fakeStream, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &fakeStream{
		ctx:     ctx,
		in:      make(chan *hostlinkv1.LinkRequest, 4),
		out:     make(chan *hostlinkv1.LinkResponse, 4),
		waiting: make(chan struct{}, 16),
	}
	s.in <- &hostlinkv1.LinkRequest{Msg: &hostlinkv1.LinkRequest_Hello{Hello: &hostlinkv1.Hello{HostId: id, Region: "r", Vcpu: 8, MemoryGb: 32}}}
	return s, cancel
}

func TestLinkRejectsHostNotMatchingCredential(t *testing.T) {
	e := newEnv()
	e.link.Auth = fakeAuth{id: "h1"}
	s, cancel := helloStream("h2")
	defer cancel()
	err := e.link.Link(s)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("Link = %v, want PermissionDenied", err)
	}
	e.core.mu.Lock()
	defer e.core.mu.Unlock()
	if len(e.core.hosts) != 0 {
		t.Fatal("rejected host was registered")
	}
}

func TestLinkAcceptsMatchingHostAndClosesAtExpiry(t *testing.T) {
	e := newEnv()
	e.link.Auth = fakeAuth{id: "h1", expires: time.Now().Add(50 * time.Millisecond)}
	s, cancel := helloStream("h1")
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.link.Link(s) }()

	// Registered while the credential is valid.
	deadline := time.Now().Add(2 * time.Second)
	for {
		e.core.mu.Lock()
		ok := e.core.hosts["h1"] != nil
		e.core.mu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("host not registered")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// After expiry, the next message from the host ends the stream.
	time.Sleep(100 * time.Millisecond)
	s.in <- heartbeat(false)
	select {
	case err := <-done:
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("Link = %v, want Unauthenticated", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream not closed after credential expiry")
	}
	e.core.mu.Lock()
	defer e.core.mu.Unlock()
	if e.core.hosts["h1"] != nil {
		t.Fatal("expired host still registered")
	}
}
