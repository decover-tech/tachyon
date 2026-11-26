package scheduler

import "testing"

func TestHostsReportsJobsAndSetDraining(t *testing.T) {
	s := New(nil)
	s.UpsertHost(host("b", "us", 16, 64))
	s.UpsertHost(host("a", "us", 16, 64))
	if _, err := s.Place(sub("j1", 4)); err != nil {
		t.Fatal(err)
	}
	hs := s.Hosts()
	if len(hs) != 2 || hs[0].ID != "a" || hs[1].ID != "b" {
		t.Fatalf("hosts = %+v", hs)
	}
	var busy, idle HostStatus
	for _, h := range hs {
		if h.Jobs == 1 {
			busy = h
		} else {
			idle = h
		}
	}
	if busy.ID == "" || busy.CPUReserved <= 0 || idle.Jobs != 0 || idle.CPUReserved != 0 {
		t.Fatalf("busy %+v idle %+v", busy, idle)
	}
	if !s.SetDraining(busy.ID, true) || s.SetDraining("nope", true) {
		t.Fatal("SetDraining existence result wrong")
	}
	// The draining host keeps its job but takes no more.
	got, err := s.Place(sub("j2", 4))
	if err != nil || got.HostID != idle.ID {
		t.Fatalf("placed on %+v err %v, want %s", got, err, idle.ID)
	}
	s.Release("j1", nil)
	for _, h := range s.Hosts() {
		if h.ID == busy.ID && (h.Jobs != 0 || !h.Draining) {
			t.Fatalf("after release %+v", h)
		}
	}
}
