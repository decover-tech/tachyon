package cache

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/decover-tech/tachyon/internal/jobapi"
)

func TestJobTokenRoundTrip(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := &Signer{Key: []byte("secret"), Now: func() time.Time { return now }}
	tok, err := s.MintJobToken("j1", prScope("4"), time.Hour)
	must(t, err)
	c, err := s.VerifyJobToken(tok)
	must(t, err)
	if c.Scope() != prScope("4") || c.JobID != "j1" {
		t.Fatalf("claims = %+v", c)
	}

	// Any change to the payload breaks the signature.
	payload, sig, _ := strings.Cut(strings.TrimPrefix(tok, tokenPrefix), ".")
	forged := tokenPrefix + payload[:len(payload)-2] + "AA." + sig
	if _, err := s.VerifyJobToken(forged); !errors.Is(err, ErrBadToken) {
		t.Errorf("forged payload: %v", err)
	}
	if _, err := (&Signer{Key: []byte("other")}).VerifyJobToken(tok); !errors.Is(err, ErrBadToken) {
		t.Errorf("wrong key: %v", err)
	}
	for _, bad := range []string{"", "tcv1.", "tcv1.abc", "nope.abc.def"} {
		if _, err := s.VerifyJobToken(bad); !errors.Is(err, ErrBadToken) {
			t.Errorf("%q: %v", bad, err)
		}
	}

	now = now.Add(2 * time.Hour)
	if _, err := s.VerifyJobToken(tok); !errors.Is(err, ErrExpiredToken) {
		t.Errorf("expired: %v", err)
	}
}

func TestMintRejectsIncompleteScope(t *testing.T) {
	s := &Signer{Key: []byte("k")}
	if _, err := s.MintJobToken("j", Scope{Trust: jobapi.Trusted}, time.Hour); err == nil {
		t.Error("minted without repo")
	}
	if _, err := s.MintJobToken("j", Scope{Repo: repo}, time.Hour); err == nil {
		t.Error("minted without trust")
	}
	if _, err := (&Signer{}).MintJobToken("j", mainScope(), time.Hour); err == nil {
		t.Error("minted without key")
	}
}

func TestBlobSignature(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := &Signer{Key: []byte("k"), Now: func() time.Time { return now }}
	q := s.blobQuery("abc", permRead, time.Minute)
	vals := map[string]string{}
	for _, kv := range strings.Split(q, "&") {
		k, v, _ := strings.Cut(kv, "=")
		vals[k] = v
	}
	must(t, s.verifyBlob("abc", permRead, vals["se"], vals["sig"]))
	if s.verifyBlob("abd", permRead, vals["se"], vals["sig"]) == nil {
		t.Error("signature accepted for another blob")
	}
	if s.verifyBlob("abc", permWrite, vals["se"], vals["sig"]) == nil {
		t.Error("read signature accepted for write")
	}
	now = now.Add(time.Hour)
	if !errors.Is(s.verifyBlob("abc", permRead, vals["se"], vals["sig"]), ErrExpiredToken) {
		t.Error("expired signature accepted")
	}
}

func TestGuestEnv(t *testing.T) {
	spec := jobapi.Spec{JobID: "j1", Repo: repo, Trust: jobapi.UntrustedFork, Ref: "refs/pull/1/merge", DefaultBranch: main}
	env, err := GuestEnv("", &Signer{Key: []byte("k")}, spec)
	if env != nil || err != nil {
		t.Fatalf("no URL: %v %v", env, err)
	}
	s := &Signer{Key: []byte("k")}
	env, err = GuestEnv("http://cache/", s, spec)
	must(t, err)
	if env[EnvURL] != "http://cache" {
		t.Errorf("url = %q", env[EnvURL])
	}
	c, err := s.VerifyJobToken(env[EnvToken])
	must(t, err)
	if c.Trust != jobapi.UntrustedFork || c.Ref != "refs/pull/1/merge" || c.DefaultBranch != main {
		t.Errorf("claims = %+v", c)
	}
	// A spec the signer can't mint for still yields the URL, plus an error.
	env, err = GuestEnv("http://cache", s, jobapi.Spec{JobID: "x"})
	if err == nil || env[EnvURL] == "" || env[EnvToken] != "" {
		t.Errorf("bad spec: %v %v", env, err)
	}
}
