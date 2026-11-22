package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	hostlinkv1 "github.com/decover-tech/tachyon/gen/hostlink/v1"
)

const testAdminToken = "0123456789abcdef0123456789abcdef-admin"

func adminDo(t *testing.T, h http.Handler, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func (e *env) admin(t *testing.T) http.Handler {
	t.Helper()
	h, err := AdminHandler(e.core, []byte(testAdminToken))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

type listResp struct {
	Hosts    []HostView     `json:"hosts"`
	Versions map[string]int `json:"versions"`
}

func (e *env) list(t *testing.T, query string) listResp {
	t.Helper()
	rec := adminDo(t, e.admin(t), "GET", "/admin/hosts"+query, testAdminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}
	var out listResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (e *env) view(t *testing.T, id string) HostView {
	t.Helper()
	for _, h := range e.list(t, "").Hosts {
		if h.ID == id {
			return h
		}
	}
	t.Fatalf("host %s not listed", id)
	return HostView{}
}

func TestAdminRequiresToken(t *testing.T) {
	e := newEnv()
	e.connect(t, "h1", "us", 16, 64)
	h := e.admin(t)
	for _, tc := range []struct{ method, path, token string }{
		{"GET", "/admin/hosts", ""},
		{"GET", "/admin/hosts", "wrong"},
		{"GET", "/admin/hosts", testAdminToken + "x"},
		{"GET", "/admin/hosts", testAdminToken[:len(testAdminToken)-1]},
		{"POST", "/admin/hosts/h1/drain", ""},
		{"POST", "/admin/hosts/h1/drain", "wrong"},
		{"POST", "/admin/hosts/h1/undrain", ""},
		// Unrouted paths do not reveal that they are unrouted.
		{"GET", "/nope", ""},
	} {
		rec := adminDo(t, h, tc.method, tc.path, tc.token)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s token=%q: %d, want 401", tc.method, tc.path, tc.token, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "h1") {
			t.Errorf("unauthenticated response leaks host data: %s", rec.Body)
		}
	}
	// A scheme other than Bearer is refused even with the right secret.
	req := httptest.NewRequest("GET", "/admin/hosts", nil)
	req.Header.Set("Authorization", testAdminToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("bare token accepted: %d", rec.Code)
	}
	if e.view(t, "h1").State != "active" {
		t.Error("rejected requests changed state")
	}
}

func TestAdminHandlerRefusesWeakToken(t *testing.T) {
	if _, err := AdminHandler(&Core{}, nil); err == nil {
		t.Error("nil token accepted")
	}
	if _, err := AdminHandler(&Core{}, []byte("short")); err == nil {
		t.Error("short token accepted")
	}
}

func TestLoadAdminToken(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	if err := os.WriteFile(good, []byte(testAdminToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tok, err := LoadAdminToken(good)
	if err != nil || string(tok) != testAdminToken {
		t.Fatalf("token = %q, %v", tok, err)
	}
	short := filepath.Join(dir, "short")
	if err := os.WriteFile(short, []byte("hunter2hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = LoadAdminToken(short)
	if err == nil {
		t.Fatal("short token accepted")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error leaks token: %v", err)
	}
	if _, err := LoadAdminToken(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing file accepted")
	}
}

func TestDrainStopsPlacementAndTellsHost(t *testing.T) {
	e := newEnv()
	h1 := e.connect(t, "h1", "us", 16, 64)
	h1.push(t, heartbeat(false))
	h2 := e.connect(t, "h2", "us", 16, 64)
	h2.push(t, heartbeat(false))

	rec := adminDo(t, e.admin(t), "POST", "/admin/hosts/h1/drain", testAdminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("drain = %d %s", rec.Code, rec.Body)
	}
	if d := h1.next(t).GetDrain(); d == nil || !d.GetDrain() {
		t.Fatalf("host not told to drain")
	}
	h2.none(t)

	// hostd has not yet acted on Drain and still reports draining=false:
	// the operator's intent wins.
	h1.push(t, heartbeat(false))
	for i, id := range []string{"j1", "j2", "j3"} {
		if err := e.core.Submit(context.Background(), e.submission(t, id, 2)); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
		if a := h2.next(t).GetAssign(); a == nil || a.GetJobId() != id {
			t.Fatalf("job %s not on h2", id)
		}
	}
	h1.none(t)
	v := e.view(t, "h1")
	if v.State != "drained" || !v.Empty || !v.DrainRequested || v.Jobs != 0 {
		t.Errorf("h1 = %+v", v)
	}
	if v := e.view(t, "h2"); v.State != "active" || v.Empty || v.Jobs != 3 {
		t.Errorf("h2 = %+v", v)
	}
}

func TestDrainedOnlyOnceJobsFinish(t *testing.T) {
	e := newEnv()
	s := e.connect(t, "h1", "us", 16, 64)
	s.push(t, heartbeat(false))
	if err := e.core.Submit(context.Background(), e.submission(t, "j1", 4)); err != nil {
		t.Fatal(err)
	}
	s.next(t) // assign

	if err := e.core.SetDrain("h1", true); err != nil {
		t.Fatal(err)
	}
	s.next(t) // drain
	if v := e.view(t, "h1"); v.State != "draining" || v.Empty || v.Jobs != 1 {
		t.Fatalf("with a running job: %+v", v)
	}
	// Further jobs have nowhere to go: the host is draining.
	if err := e.core.Submit(context.Background(), e.submission(t, "j2", 2)); err == nil {
		if a := s.next(t).GetAssign(); a != nil {
			t.Fatalf("job placed on draining host: %v", a)
		}
	}
	s.push(t, &hostlinkv1.LinkRequest{Msg: &hostlinkv1.LinkRequest_JobResult{JobResult: &hostlinkv1.JobResult{
		JobId: "j1", Outcome: hostlinkv1.JobResult_OUTCOME_SUCCEEDED,
	}}})
	if v := e.view(t, "h1"); v.State != "drained" || !v.Empty || v.Jobs != 0 {
		t.Fatalf("after the job finished: %+v", v)
	}
}

func TestUndrainResumesPlacement(t *testing.T) {
	e := newEnv()
	s := e.connect(t, "h1", "us", 16, 64)
	s.push(t, heartbeat(false))
	if err := e.core.SetDrain("h1", true); err != nil {
		t.Fatal(err)
	}
	s.next(t)
	rec := adminDo(t, e.admin(t), "POST", "/admin/hosts/h1/undrain", testAdminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("undrain = %d %s", rec.Code, rec.Body)
	}
	if d := s.next(t).GetDrain(); d == nil || d.GetDrain() {
		t.Fatal("host not told to undrain")
	}
	// A heartbeat sent before hostd saw the undrain must not re-drain it
	// for good; the next one reflects the new state.
	s.push(t, heartbeat(false))
	if v := e.view(t, "h1"); v.State != "active" || v.DrainRequested {
		t.Fatalf("h1 = %+v", v)
	}
	if err := e.core.Submit(context.Background(), e.submission(t, "j1", 2)); err != nil {
		t.Fatal(err)
	}
	if a := s.next(t).GetAssign(); a == nil {
		t.Fatal("job not placed after undrain")
	}
}

func TestDrainSurvivesHostRestart(t *testing.T) {
	e := newEnv()
	s := e.connect(t, "h1", "us", 16, 64)
	s.push(t, heartbeat(false))
	if err := e.core.SetDrain("h1", true); err != nil {
		t.Fatal(err)
	}
	s.next(t)
	s.disconnect()

	// The host comes back after an upgrade with a fresh hostd that is not
	// draining: it is drained again before any job can be placed.
	s2 := e.connect(t, "h1", "us", 16, 64)
	if d := s2.next(t).GetDrain(); d == nil || !d.GetDrain() {
		t.Fatal("reconnected host not told to drain")
	}
	if v := e.view(t, "h1"); v.State != "drained" || !v.Connected {
		t.Fatalf("h1 = %+v", v)
	}
	if err := e.core.Submit(context.Background(), e.submission(t, "j1", 2)); err == nil {
		if a := s2.next(t).GetAssign(); a != nil {
			t.Fatal("job placed on reconnected drained host")
		}
	}
	if err := e.core.SetDrain("h1", false); err != nil {
		t.Fatal(err)
	}
	if v := e.view(t, "h1"); v.State != "active" {
		t.Fatalf("h1 = %+v", v)
	}
}

func TestDrainUnknownHost(t *testing.T) {
	e := newEnv()
	if err := e.core.SetDrain("ghost", true); !errors.Is(err, ErrUnknownHost) {
		t.Fatalf("err = %v", err)
	}
	rec := adminDo(t, e.admin(t), "POST", "/admin/hosts/ghost/drain", testAdminToken)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("drain ghost = %d", rec.Code)
	}
	if e.core.drainWanted["ghost"] {
		t.Error("drain recorded for unknown host")
	}
}

func TestAdminListsVersionAndGroup(t *testing.T) {
	e := newEnv()
	e.connectHello(t, &hostlinkv1.Hello{HostId: "c1", Region: "us", Arch: "x86_64", Vcpu: 8, MemoryGb: 32, HostdVersion: "0.2.0", HostGroup: "canary"})
	e.connectHello(t, &hostlinkv1.Hello{HostId: "s1", Region: "us", Arch: "x86_64", Vcpu: 8, MemoryGb: 32, HostdVersion: "0.1.0", HostGroup: "stable"})
	e.connectHello(t, &hostlinkv1.Hello{HostId: "s2", Region: "us", Arch: "x86_64", Vcpu: 8, MemoryGb: 32, HostdVersion: "0.1.0", HostGroup: "stable"})

	all := e.list(t, "")
	if len(all.Hosts) != 3 || all.Versions["0.2.0"] != 1 || all.Versions["0.1.0"] != 2 {
		t.Fatalf("list = %+v", all)
	}
	if all.Hosts[0].ID != "c1" || all.Hosts[0].Group != "canary" || all.Hosts[0].HostdVersion != "0.2.0" || !all.Hosts[0].Connected {
		t.Errorf("c1 = %+v", all.Hosts[0])
	}
	canary := e.list(t, "?group=canary")
	if len(canary.Hosts) != 1 || canary.Hosts[0].ID != "c1" {
		t.Errorf("group filter = %+v", canary)
	}
	old := e.list(t, "?version=0.1.0")
	if len(old.Hosts) != 2 {
		t.Errorf("version filter = %+v", old)
	}
	if none := e.list(t, "?group=nope"); none.Hosts == nil || len(none.Hosts) != 0 {
		t.Errorf("empty filter should list [] = %+v", none)
	}
}

func TestVersionKnownAfterDisconnect(t *testing.T) {
	e := newEnv()
	s := e.connectHello(t, &hostlinkv1.Hello{HostId: "c1", Region: "us", Arch: "x86_64", Vcpu: 8, MemoryGb: 32, HostdVersion: "0.2.0", HostGroup: "canary"})
	s.disconnect()
	v := e.view(t, "c1")
	if v.Connected || v.HostdVersion != "0.2.0" || v.Group != "canary" {
		t.Errorf("c1 = %+v", v)
	}
}
