package secrets

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestOpen(t *testing.T) {
	s, err := Open(Config{Backend: "mem", JITTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	m, ok := s.(*Mem)
	if !ok || m.Policy.TTL("jit/x") != time.Minute || m.Policy.TTL("y") != DefaultTTL {
		t.Fatalf("mem store = %#v", s)
	}

	s, err = Open(Config{Backend: "vault", VaultAddr: "https://v", VaultToken: Redacted("t"), DefaultTTL: 2 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	v, ok := s.(*Vault)
	if !ok || v.Policy.TTL("jit/x") != DefaultJITTTL || v.Policy.TTL("y") != 2*time.Hour {
		t.Fatalf("vault store = %#v", s)
	}

	if out := fmt.Sprintf("%v %+v %#v", v, v, *v); strings.Contains(out, `"t"`) || !strings.Contains(out, "REDACTED") {
		t.Fatalf("token printed: %s", out)
	}

	for _, bad := range []Config{
		{},
		{Backend: "s3"},
		{Backend: "vault", VaultToken: Redacted("t")},
		{Backend: "vault", VaultAddr: "https://v"},
		{Backend: "mem", JITTTL: -time.Second},
	} {
		if _, err := Open(bad); err == nil {
			t.Errorf("Open(%+v) succeeded", bad)
		}
	}
}
