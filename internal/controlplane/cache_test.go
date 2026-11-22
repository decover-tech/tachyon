package controlplane

import (
	"context"
	"testing"

	"github.com/decover-tech/tachyon/internal/cache"
	"github.com/decover-tech/tachyon/internal/jobapi"
)

func TestAssignCarriesScopedCacheToken(t *testing.T) {
	e := newEnv()
	signer := &cache.Signer{Key: []byte("k")}
	e.core.CacheURL = "http://cache.us.internal/"
	e.core.CacheSigner = signer
	s := e.connect(t, "h1", "us", 16, 64)

	sub := e.submission(t, "j1", 2)
	sub.Spec.Trust = jobapi.Untrusted
	sub.Spec.Ref, sub.Spec.BaseRef, sub.Spec.DefaultBranch = "refs/pull/7/merge", "refs/heads/main", "refs/heads/main"
	if err := e.core.Submit(context.Background(), sub); err != nil {
		t.Fatal(err)
	}
	envs := s.next(t).GetAssign().GetGuest().GetEnv()
	if envs[cache.EnvURL] != "http://cache.us.internal" {
		t.Errorf("cache url = %q", envs[cache.EnvURL])
	}
	c, err := signer.VerifyJobToken(envs[cache.EnvToken])
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	want := cache.Scope{Repo: "github.com/acme/api", Ref: "refs/pull/7/merge", BaseRef: "refs/heads/main", DefaultBranch: "refs/heads/main", Trust: jobapi.Untrusted}
	if c.Scope() != want || c.JobID != "j1" {
		t.Errorf("claims = %+v", c)
	}
}

func TestAssignWithoutCacheService(t *testing.T) {
	e := newEnv()
	e.core.CacheSigner = &cache.Signer{Key: []byte("k")}
	s := e.connect(t, "h1", "us", 16, 64)
	if err := e.core.Submit(context.Background(), e.submission(t, "j1", 2)); err != nil {
		t.Fatal(err)
	}
	envs := s.next(t).GetAssign().GetGuest().GetEnv()
	if _, ok := envs[cache.EnvToken]; ok {
		t.Error("token sent with no cache URL configured")
	}
}
