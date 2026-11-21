package scheduler

import (
	"errors"
	"testing"
)

func TestRestoreHostAndAllocation(t *testing.T) {
	s := New(nil)
	h := host("a", "us", 4, 16)
	h.Draining = true // not reconnected yet
	s.RestoreHost(h, 1.5)
	if got := s.Ratio("a"); got != 1.5 {
		t.Fatalf("ratio = %v", got)
	}
	if err := s.RestoreAllocation(sub("j1", 4), "a", 4, 16); err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreAllocation(sub("j1", 4), "a", 4, 16); err != nil {
		t.Fatalf("restoring twice: %v", err)
	}
	if err := s.RestoreAllocation(sub("j2", 2), "missing", 2, 8); err == nil {
		t.Fatal("allocation on unknown host accepted")
	}
	// Restored hosts take no new work until they reconnect.
	if _, err := s.Place(sub("j3", 2)); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("placed on draining restored host: %v", err)
	}
	// Reconnect: the heartbeat keeps the restored reservation, so the host
	// is full.
	h.Draining = false
	s.UpsertHost(h)
	if _, err := s.Place(sub("j3", 4)); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("restored reservation lost: %v", err)
	}
	s.Release("j1", nil)
	if _, err := s.Place(sub("j3", 4)); err != nil {
		t.Fatalf("release of restored job: %v", err)
	}
	// RestoreHost never overwrites a connected host.
	s.RestoreHost(host("a", "eu", 2, 8), 1.0)
	if s.Ratio("a") != 1.5 {
		t.Error("RestoreHost overwrote a live host")
	}
	s.RestoreHost(host("b", "us", 4, 16), 99)
	if s.Ratio("b") != StartRatio {
		t.Error("out-of-range ratio not reset")
	}
}
