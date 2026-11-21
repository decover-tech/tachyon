package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestRefRoundTrip(t *testing.T) {
	ref, err := NewRef("jit/tachyon-gh_1-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ref, "secret://jit/tachyon-gh_1-1#") {
		t.Fatalf("ref = %q", ref)
	}
	path, id, err := ParseRef(ref)
	if err != nil || path != "jit/tachyon-gh_1-1" || len(id) != 32 {
		t.Fatalf("ParseRef = %q %q %v", path, id, err)
	}
	other, _ := NewRef("jit/tachyon-gh_1-1")
	if other == ref {
		t.Fatal("refs for the same path must differ")
	}
}

func TestParseRefRejects(t *testing.T) {
	for _, ref := range []string{
		"",
		"jit/x#0123456789abcdef0123456789abcdef",
		"secret://jit/x",
		"secret://jit/x#",
		"secret://jit/x#zz",
		"secret://../x#0123456789abcdef0123456789abcdef",
		"secret://jit//x#0123456789abcdef0123456789abcdef",
		"secret:///jit#0123456789abcdef0123456789abcdef",
		"secret://jit/x?y#0123456789abcdef0123456789abcdef",
		"secret://jit/x%2F#0123456789abcdef0123456789abcdef",
	} {
		if _, _, err := ParseRef(ref); !errors.Is(err, ErrInvalidRef) {
			t.Errorf("ParseRef(%q) err = %v", ref, err)
		}
	}
	if _, err := NewRef("a/../b"); !errors.Is(err, ErrInvalidRef) {
		t.Errorf("NewRef with .. err = %v", err)
	}
}

func TestPolicyTTL(t *testing.T) {
	var zero Policy
	if got := zero.TTL("jit/r"); got != DefaultJITTTL {
		t.Errorf("zero policy jit TTL = %v", got)
	}
	if got := zero.TTL("token/x"); got != DefaultTTL {
		t.Errorf("zero policy default TTL = %v", got)
	}
	p := Policy{Default: 5 * time.Minute, Prefixes: map[string]time.Duration{
		"jit/":      time.Minute,
		"jit/long/": time.Hour,
	}}
	for path, want := range map[string]time.Duration{
		"jit/a":       time.Minute,
		"jit/long/a":  time.Hour,
		"other/a":     5 * time.Minute,
		"jitter/a":    5 * time.Minute,
		"jit/longx/a": time.Minute,
	} {
		if got := p.TTL(path); got != want {
			t.Errorf("TTL(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestRedactedNeverPrints(t *testing.T) {
	const s = "super-secret-jit-config"
	r := Redacted(s)
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	log.Info("x", "cfg", r)
	slog.New(slog.NewTextHandler(&logs, nil)).Info("x", "cfg", r)
	j, _ := json.Marshal(struct{ Cfg Redacted }{r})
	out := []string{
		logs.String(),
		string(j),
		fmt.Sprint(r),
		fmt.Sprintf("%v %+v %#v %s %q %x", r, r, r, r, r, r),
		fmt.Sprintf("%+v", struct{ Cfg Redacted }{r}),
	}
	for _, o := range out {
		if strings.Contains(o, s) || strings.Contains(o, fmt.Sprintf("%x", s)) {
			t.Errorf("secret leaked: %s", o)
		}
		if !strings.Contains(o, "REDACTED") {
			t.Errorf("no redaction marker: %s", o)
		}
	}
	if string(r.Reveal()) != s {
		t.Error("Reveal lost the value")
	}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

// storeContract runs the behavior every Store must have.
func storeContract(t *testing.T, s Store, c *clock) {
	t.Helper()
	ctx := context.Background()
	const val = "jit-config-value"

	ref, err := s.Put(ctx, "jit/runner-1", []byte(val))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ref, val) {
		t.Fatal("reference contains the value")
	}
	got, err := s.Get(ctx, ref)
	if err != nil || string(got) != val {
		t.Fatalf("Get = %q, %v", got, err)
	}

	// Deleted values are gone; deleting twice is fine.
	if err := s.Delete(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, ref); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if _, err := s.Get(ctx, ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete err = %v", err)
	}

	// JIT configs expire after their short TTL.
	ref, err = s.Put(ctx, "jit/runner-2", []byte(val))
	if err != nil {
		t.Fatal(err)
	}
	c.advance(DefaultJITTTL - time.Second)
	if _, err := s.Get(ctx, ref); err != nil {
		t.Fatalf("before TTL: %v", err)
	}
	c.advance(2 * time.Second)
	_, err = s.Get(ctx, ref)
	if !errors.Is(err, ErrExpired) && !errors.Is(err, ErrNotFound) {
		t.Fatalf("after TTL err = %v", err)
	}
	if err != nil && strings.Contains(err.Error(), val) {
		t.Fatal("error contains the value")
	}

	// Other paths get the longer default.
	ref, err = s.Put(ctx, "token/x", []byte(val))
	if err != nil {
		t.Fatal(err)
	}
	c.advance(DefaultJITTTL + time.Minute)
	if _, err := s.Get(ctx, ref); err != nil {
		t.Fatalf("token before default TTL: %v", err)
	}

	// Unknown and malformed refs.
	if _, err := s.Get(ctx, "secret://jit/none#0123456789abcdef0123456789abcdef"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown ref err = %v", err)
	}
	if _, err := s.Put(ctx, "../escape", []byte(val)); !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("bad path err = %v", err)
	}
}

func TestMemStore(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)}
	storeContract(t, &Mem{Now: c.now}, c)
}

func TestMemSweep(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)}
	s := &Mem{Now: c.now}
	ctx := context.Background()
	if _, err := s.Put(ctx, "jit/a", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, "token/b", []byte("v")); err != nil {
		t.Fatal(err)
	}
	c.advance(DefaultJITTTL)
	if n := s.Sweep(); n != 1 || s.Len() != 1 {
		t.Fatalf("Sweep = %d, Len = %d", n, s.Len())
	}
}

func TestMemCopiesValues(t *testing.T) {
	s := &Mem{}
	ctx := context.Background()
	in := []byte("abc")
	ref, _ := s.Put(ctx, "jit/a", in)
	in[0] = 'x'
	out, _ := s.Get(ctx, ref)
	out[1] = 'y'
	again, _ := s.Get(ctx, ref)
	if string(again) != "abc" {
		t.Fatalf("stored value aliased caller memory: %q", again)
	}
}
