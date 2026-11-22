package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeKV is a minimal Vault KV v2 engine: metadata and data endpoints,
// check-and-set, max_versions and delete_version_after against a fake clock.
type fakeKV struct {
	t     *testing.T
	token string
	now   func() time.Time

	mu       sync.Mutex
	meta     map[string]time.Duration // key -> delete_version_after
	data     map[string]json.RawMessage
	written  map[string]time.Time
	requests []string
	bodies   []string
	fail     map[string]int // "METHOD kind" -> status to return

	deletionTime string // if set, overrides deletion_time on reads
}

func newFakeKV(t *testing.T, now func() time.Time) (*fakeKV, *httptest.Server) {
	f := &fakeKV{t: t, token: "test-token", now: now,
		meta: map[string]time.Duration{}, data: map[string]json.RawMessage{},
		written: map[string]time.Time{}, fail: map[string]int{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeKV) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := readAll(r)
	f.bodies = append(f.bodies, body)
	if r.Header.Get("X-Vault-Token") != f.token {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"errors":["permission denied"]}`))
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/v1/kv/")
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	kind, key, _ := strings.Cut(rest, "/")
	f.requests = append(f.requests, r.Method+" "+kind+" "+key)
	if code := f.fail[r.Method+" "+kind]; code != 0 {
		w.WriteHeader(code)
		w.Write([]byte(`{"errors":["` + body + `"]}`)) // echo the body, as a hostile server might
		return
	}
	// Server-side delete_version_after.
	if ttl, ok := f.meta[key]; ok && ttl > 0 {
		if at, ok := f.written[key]; ok && !f.now().Before(at.Add(ttl)) {
			delete(f.data, key)
		}
	}
	switch {
	case kind == "metadata" && r.Method == http.MethodPost:
		var m struct {
			MaxVersions        int    `json:"max_versions"`
			DeleteVersionAfter string `json:"delete_version_after"`
		}
		if err := json.Unmarshal([]byte(body), &m); err != nil || m.MaxVersions != 1 {
			f.t.Errorf("bad metadata body %s", body)
		}
		d, err := time.ParseDuration(m.DeleteVersionAfter)
		if err != nil {
			f.t.Errorf("bad delete_version_after %q", m.DeleteVersionAfter)
		}
		f.meta[key] = d
		w.WriteHeader(http.StatusNoContent)
	case kind == "metadata" && r.Method == http.MethodDelete:
		delete(f.meta, key)
		delete(f.data, key)
		w.WriteHeader(http.StatusNoContent)
	case kind == "data" && r.Method == http.MethodPost:
		var d struct {
			Options struct {
				CAS *int `json:"cas"`
			} `json:"options"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal([]byte(body), &d); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if d.Options.CAS == nil || *d.Options.CAS != 0 {
			f.t.Errorf("Put must use cas=0, got %s", body)
		}
		if _, exists := f.data[key]; exists {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.data[key] = d.Data
		f.written[key] = f.now()
		w.Write([]byte(`{"data":{"version":1}}`))
	case kind == "data" && r.Method == http.MethodGet:
		v, ok := f.data[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"errors":[]}`))
			return
		}
		// Like real KV v2, a live version carries a future deletion_time
		// when delete_version_after is set.
		deletion := ""
		if ttl := f.meta[key]; ttl > 0 {
			deletion = f.written[key].Add(ttl).UTC().Format(time.RFC3339Nano)
		}
		if f.deletionTime != "" {
			deletion = f.deletionTime
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"data":     v,
			"metadata": map[string]any{"version": 1, "deletion_time": deletion, "destroyed": false},
		}})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func readAll(r *http.Request) (string, error) {
	var b bytes.Buffer
	_, err := b.ReadFrom(r.Body)
	return b.String(), err
}

func TestVaultStore(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)}
	f, srv := newFakeKV(t, c.now)
	v := &Vault{Addr: srv.URL, Mount: "kv", Prefix: "tachyon/prod", Token: Redacted(f.token), Now: c.now}
	storeContract(t, v, c)

	// Keys are namespaced by prefix, path and the ref's random id.
	for _, r := range f.requests {
		if !strings.Contains(r, " tachyon/prod/") {
			t.Errorf("request outside prefix: %s", r)
		}
	}
}

func TestVaultServerSideExpiry(t *testing.T) {
	// The server's delete_version_after removes the value even if the client
	// clock never checks expires_at.
	c := &clock{t: time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)}
	f, srv := newFakeKV(t, c.now)
	v := &Vault{Addr: srv.URL, Mount: "kv", Token: Redacted(f.token)} // real clock
	ref, err := v.Put(context.Background(), "jit/r", []byte("v"))
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	var ttl time.Duration
	for _, d := range f.meta {
		ttl = d
	}
	f.mu.Unlock()
	if ttl != DefaultJITTTL {
		t.Fatalf("delete_version_after = %v, want %v", ttl, DefaultJITTTL)
	}
	c.advance(DefaultJITTTL)
	if _, err := v.Get(context.Background(), ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestVaultDeletionTime(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)}
	f, srv := newFakeKV(t, c.now)
	v := &Vault{Addr: srv.URL, Mount: "kv", Token: Redacted(f.token), Now: c.now}
	ctx := context.Background()
	ref, err := v.Put(ctx, "jit/r", []byte("v"))
	if err != nil {
		t.Fatal(err)
	}
	// A live version with a future deletion_time, as real Vault returns it,
	// resolves.
	if got, err := v.Get(ctx, ref); err != nil || string(got) != "v" {
		t.Fatalf("live value: got %q, err %v", got, err)
	}
	// A deletion_time in the past means the version was deleted, even if the
	// server still answers 200 with data.
	f.mu.Lock()
	f.deletionTime = c.now().Add(-time.Second).Format(time.RFC3339Nano)
	f.mu.Unlock()
	if _, err := v.Get(ctx, ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("past deletion_time: err = %v", err)
	}
	// An unparseable deletion_time fails closed.
	f.mu.Lock()
	f.deletionTime = "not-a-time"
	f.mu.Unlock()
	if _, err := v.Get(ctx, ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad deletion_time: err = %v", err)
	}
}

func TestVaultTokenFile(t *testing.T) {
	f, srv := newFakeKV(t, time.Now)
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("wrong\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	v := &Vault{Addr: srv.URL, Mount: "kv", TokenFile: file}
	if _, err := v.Put(context.Background(), "jit/r", []byte("v")); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("wrong token err = %v", err)
	}
	// Rotated by an agent: picked up on the next request.
	if err := os.WriteFile(file, []byte(f.token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Put(context.Background(), "jit/r", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Vault{Addr: srv.URL}).Put(context.Background(), "jit/r", []byte("v")); err == nil {
		t.Fatal("no token should fail")
	}
}

func TestVaultErrorsNeverCarrySecrets(t *testing.T) {
	const val = "ghs_super_secret_jit"
	f, srv := newFakeKV(t, time.Now)
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	v := &Vault{Addr: srv.URL, Mount: "kv", Token: Redacted(f.token)}
	ctx := context.Background()

	// A failed data write echoes the request body back; the error must not.
	f.fail["POST data"] = http.StatusInternalServerError
	_, err := v.Put(ctx, "jit/r", []byte(val))
	if err == nil {
		t.Fatal("want error")
	}
	log.Error("put", "err", err)
	// The half-written key was cleaned up.
	f.mu.Lock()
	last := f.requests[len(f.requests)-1]
	n := len(f.meta)
	f.mu.Unlock()
	if !strings.HasPrefix(last, "DELETE metadata ") || n != 0 {
		t.Fatalf("no cleanup after failed write: last=%s meta=%d", last, n)
	}
	delete(f.fail, "POST data")

	ref, err := v.Put(ctx, "jit/r", []byte(val))
	if err != nil {
		t.Fatal(err)
	}
	f.fail["GET data"] = http.StatusInternalServerError
	_, err = v.Get(ctx, ref)
	if err == nil {
		t.Fatal("want error")
	}
	log.Error("get", "err", err)

	if strings.Contains(logs.String(), val) || strings.Contains(logs.String(), "Z2hzX3N1cGVy") {
		t.Fatalf("secret in logs: %s", logs.String())
	}
	// And the value only ever crossed the wire base64-encoded in a data write.
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range f.bodies {
		if strings.Contains(b, val) {
			t.Fatalf("plaintext value on the wire: %s", b)
		}
	}
}

func TestVaultNamespaceHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Vault-Namespace")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	v := &Vault{Addr: srv.URL, Token: Redacted("t"), Namespace: "team-a"}
	if err := v.Delete(context.Background(), "secret://jit/r#0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if got != "team-a" {
		t.Fatalf("namespace header = %q", got)
	}
}
