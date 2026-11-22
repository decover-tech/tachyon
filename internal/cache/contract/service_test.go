//go:build contract

// This file needs the cache service (cache.NewServer, cache.Signer), so it
// builds only with -tags contract, which is how the daily workflow runs it.

package contract

import (
	"crypto/rand"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/decover-tech/tachyon/internal/cache"
)

func init() { startService = inProcessService }

// inProcessService serves the real cache service, with an in-memory blob
// store, on a loopback port that Node can reach.
func inProcessService(t *testing.T, target Target) Target {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	signer := &cache.Signer{Key: key}
	srv := cache.NewServer(&cache.Index{QuotaBytes: cache.DefaultQuotaBytes}, &cache.MemStore{}, signer, "")
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	srv.PublicURL = hs.URL
	target.URL = hs.URL
	for _, j := range target.Jobs.All() {
		sc := cache.Scope{Repo: target.Repo, Ref: j.Ref, BaseRef: j.BaseRef, DefaultBranch: target.DefaultBranch, Trust: j.Trust}
		tok, err := signer.MintJobToken("contract-"+j.Name, sc, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		j.Token = tok
	}
	return target
}
