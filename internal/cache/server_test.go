package cache

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/decover-tech/tachyon/internal/jobapi"
)

type harness struct {
	t      *testing.T
	srv    *Server
	http   *httptest.Server
	store  *MemStore
	signer *Signer
}

func newHarness(t *testing.T, quota int64) *harness {
	t.Helper()
	store := &MemStore{}
	signer := &Signer{Key: []byte("regional-key")}
	ix, _ := newIndex(quota)
	h := &harness{t: t, store: store, signer: signer}
	h.srv = NewServer(ix, store, signer, "")
	h.http = httptest.NewServer(h.srv.Handler())
	h.srv.PublicURL = h.http.URL
	t.Cleanup(h.http.Close)
	return h
}

func (h *harness) token(s Scope) string {
	h.t.Helper()
	tok, err := h.signer.MintJobToken("job", s, time.Hour)
	must(h.t, err)
	return tok
}

// call makes a Twirp JSON call and returns the HTTP status and decoded body.
func (h *harness) call(tok, method string, req any) (int, map[string]any) {
	h.t.Helper()
	b, _ := json.Marshal(req)
	r, _ := http.NewRequest(http.MethodPost, h.http.URL+TwirpPrefix+method, bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	if tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(r)
	must(h.t, err)
	defer resp.Body.Close()
	var out map[string]any
	must(h.t, json.NewDecoder(resp.Body).Decode(&out))
	return resp.StatusCode, out
}

func (h *harness) ok(tok, method string, req any) map[string]any {
	h.t.Helper()
	status, out := h.call(tok, method, req)
	if status != http.StatusOK {
		h.t.Fatalf("%s: status %d %v", method, status, out)
	}
	return out
}

func (h *harness) put(url string, body []byte, hdr map[string]string) int {
	h.t.Helper()
	r, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(r)
	must(h.t, err)
	resp.Body.Close()
	return resp.StatusCode
}

func (h *harness) get(url string, hdr map[string]string) (int, []byte) {
	h.t.Helper()
	r, _ := http.NewRequest(http.MethodGet, url, nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(r)
	must(h.t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// save runs the toolkit's save sequence with a single-shot upload.
func (h *harness) save(tok, key string, data []byte) map[string]any {
	h.t.Helper()
	c := h.ok(tok, "CreateCacheEntry", map[string]any{"key": key, "version": "v1"})
	if c["ok"] != true {
		return c
	}
	if st := h.put(c["signedUploadUrl"].(string), data, map[string]string{"x-ms-blob-type": "BlockBlob"}); st != http.StatusCreated {
		h.t.Fatalf("upload status %d", st)
	}
	return h.ok(tok, "FinalizeCacheEntryUpload", map[string]any{"key": key, "version": "v1", "size_bytes": fmt.Sprint(len(data))})
}

// restore returns the restored bytes and matched key, or ok=false on a miss.
func (h *harness) restore(tok, key string, restoreKeys ...string) ([]byte, string, bool) {
	h.t.Helper()
	out := h.ok(tok, "GetCacheEntryDownloadURL", map[string]any{"key": key, "version": "v1", "restore_keys": restoreKeys})
	if out["ok"] != true {
		return nil, "", false
	}
	st, b := h.get(out["signedDownloadUrl"].(string), nil)
	if st != http.StatusOK {
		h.t.Fatalf("download status %d", st)
	}
	return b, out["matchedKey"].(string), true
}

func TestSaveAndRestore(t *testing.T) {
	h := newHarness(t, 0)
	main := h.token(mainScope())
	out := h.save(main, "npm-linux-abc", []byte("archive"))
	if out["ok"] != true || out["entryId"] != "1" {
		t.Fatalf("finalize = %v", out)
	}
	b, matched, ok := h.restore(main, "npm-linux-zzz", "npm-linux-")
	if !ok || string(b) != "archive" || matched != "npm-linux-abc" {
		t.Fatalf("restore = %q %q %v", b, matched, ok)
	}
	if _, _, ok := h.restore(main, "pip-x"); ok {
		t.Error("unrelated key hit")
	}

	// Ranged reads, as the toolkit's concurrent downloader makes.
	url := h.ok(main, "GetCacheEntryDownloadURL", map[string]any{"key": "npm-linux-abc", "version": "v1"})["signedDownloadUrl"].(string)
	st, part := h.get(url, map[string]string{"Range": "bytes=2-4"})
	if st != http.StatusPartialContent || string(part) != "chi" {
		t.Errorf("range = %d %q", st, part)
	}
}

func TestCamelCaseRequestFields(t *testing.T) {
	h := newHarness(t, 0)
	tok := h.token(mainScope())
	h.save(tok, "go-abc", []byte("x"))
	out := h.ok(tok, "GetCacheEntryDownloadURL", map[string]any{"key": "miss", "version": "v1", "restoreKeys": []string{"go-"}})
	if out["ok"] != true || out["matchedKey"] != "go-abc" {
		t.Errorf("camelCase restoreKeys: %v", out)
	}
	c := h.ok(tok, "CreateCacheEntry", map[string]any{"key": "n", "version": "v1"})
	h.put(c["signedUploadUrl"].(string), []byte("abc"), nil)
	out = h.ok(tok, "FinalizeCacheEntryUpload", map[string]any{"key": "n", "version": "v1", "sizeBytes": 3})
	if out["ok"] != true {
		t.Errorf("numeric sizeBytes: %v", out)
	}
}

func TestBlockUpload(t *testing.T) {
	h := newHarness(t, 0)
	tok := h.token(mainScope())
	c := h.ok(tok, "CreateCacheEntry", map[string]any{"key": "k", "version": "v1"})
	url := c["signedUploadUrl"].(string)
	b1 := base64.StdEncoding.EncodeToString([]byte("block-0001"))
	b2 := base64.StdEncoding.EncodeToString([]byte("block-0002"))
	// Azure's SDK appends comp and blockid to the signed URL; blocks may
	// arrive out of order.
	for _, blk := range []struct{ id, data string }{{b2, "world"}, {b1, "hello "}} {
		if st := h.put(url+"&comp=block&blockid="+strings.ReplaceAll(blk.id, "=", "%3D"), []byte(blk.data), nil); st != http.StatusCreated {
			t.Fatalf("put block: %d", st)
		}
	}
	list := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?><BlockList><Latest>%s</Latest><Uncommitted>%s</Uncommitted></BlockList>`, b1, b2)
	if st := h.put(url+"&comp=blocklist", []byte(list), nil); st != http.StatusCreated {
		t.Fatalf("put block list: %d", st)
	}
	out := h.ok(tok, "FinalizeCacheEntryUpload", map[string]any{"key": "k", "version": "v1", "size_bytes": "11"})
	if out["ok"] != true {
		t.Fatalf("finalize = %v", out)
	}
	b, _, ok := h.restore(tok, "k")
	if !ok || string(b) != "hello world" {
		t.Fatalf("restore = %q", b)
	}
	if h.store.Len() != 1 {
		t.Errorf("staged blocks left behind: %d blobs", h.store.Len())
	}

	// A block list naming an unstaged block is rejected.
	c = h.ok(tok, "CreateCacheEntry", map[string]any{"key": "k2", "version": "v1"})
	if st := h.put(c["signedUploadUrl"].(string)+"&comp=blocklist", []byte(`<BlockList><Latest>bm9wZQ==</Latest></BlockList>`), nil); st != http.StatusBadRequest {
		t.Errorf("unstaged block list: %d", st)
	}
}

func TestScopingOverTheWire(t *testing.T) {
	h := newHarness(t, 0)
	main, pr1, pr2 := h.token(mainScope()), h.token(prScope("1")), h.token(prScope("2"))
	fork := prScope("3")
	fork.Trust = jobapi.UntrustedFork
	forkTok := h.token(fork)

	h.save(main, "k-main", []byte("m"))
	h.save(pr1, "k-pr1", []byte("p"))

	if _, _, ok := h.restore(pr1, "k-main"); !ok {
		t.Error("PR can't read default branch")
	}
	if _, _, ok := h.restore(pr2, "k-pr1"); ok {
		t.Error("PR 2 read PR 1's entry")
	}
	if _, _, ok := h.restore(main, "k-pr1"); ok {
		t.Error("default branch read a PR entry")
	}
	if _, _, ok := h.restore(forkTok, "k-main"); !ok {
		t.Error("fork can't read default branch")
	}
	out := h.ok(forkTok, "CreateCacheEntry", map[string]any{"key": "evil", "version": "v1"})
	if out["ok"] != false || out["signedUploadUrl"] != nil || !strings.Contains(out["message"].(string), "fork") {
		t.Errorf("fork create = %v", out)
	}
	// The request's own metadata can't widen the scope: only the token counts.
	out = h.ok(pr1, "CreateCacheEntry", map[string]any{"key": "k-main2", "version": "v1",
		"metadata": map[string]any{"scope": []map[string]any{{"scope": main, "permission": "3"}}}})
	h.put(out["signedUploadUrl"].(string), []byte("x"), nil)
	h.ok(pr1, "FinalizeCacheEntryUpload", map[string]any{"key": "k-main2", "version": "v1"})
	if _, _, ok := h.restore(main, "k-main2"); ok {
		t.Error("PR wrote into the default branch via metadata")
	}
}

func TestDuplicateAndConcurrentCreate(t *testing.T) {
	h := newHarness(t, 0)
	tok := h.token(mainScope())
	c := h.ok(tok, "CreateCacheEntry", map[string]any{"key": "k", "version": "v1"})
	if again := h.ok(tok, "CreateCacheEntry", map[string]any{"key": "k", "version": "v1"}); again["ok"] != false {
		t.Errorf("second reservation while first is open: %v", again)
	}
	h.put(c["signedUploadUrl"].(string), []byte("x"), nil)
	h.ok(tok, "FinalizeCacheEntryUpload", map[string]any{"key": "k", "version": "v1"})
	if again := h.ok(tok, "CreateCacheEntry", map[string]any{"key": "k", "version": "v1"}); again["ok"] != false {
		t.Errorf("create of existing entry: %v", again)
	}
	// Entries are immutable: the old upload URL is closed after finalize.
	if st := h.put(c["signedUploadUrl"].(string), []byte("overwrite"), nil); st != http.StatusConflict {
		t.Errorf("upload after finalize: %d", st)
	}
	// Finalize without a reservation fails softly.
	if out := h.ok(tok, "FinalizeCacheEntryUpload", map[string]any{"key": "nope", "version": "v1"}); out["ok"] != false {
		t.Errorf("finalize without reservation: %v", out)
	}
}

func TestFinalizeRejectsSizeMismatchAndMissingUpload(t *testing.T) {
	h := newHarness(t, 0)
	tok := h.token(mainScope())
	c := h.ok(tok, "CreateCacheEntry", map[string]any{"key": "k", "version": "v1"})
	h.put(c["signedUploadUrl"].(string), []byte("abc"), nil)
	if out := h.ok(tok, "FinalizeCacheEntryUpload", map[string]any{"key": "k", "version": "v1", "size_bytes": "99"}); out["ok"] != false {
		t.Errorf("size mismatch accepted: %v", out)
	}
	if h.store.Len() != 0 {
		t.Error("rejected blob not deleted")
	}
	h.ok(tok, "CreateCacheEntry", map[string]any{"key": "k2", "version": "v1"})
	if out := h.ok(tok, "FinalizeCacheEntryUpload", map[string]any{"key": "k2", "version": "v1"}); out["ok"] != false {
		t.Errorf("finalize with nothing uploaded: %v", out)
	}
}

func TestAuthAndSignedURLs(t *testing.T) {
	h := newHarness(t, 0)
	if st, out := h.call("", "CreateCacheEntry", map[string]any{"key": "k", "version": "v"}); st != http.StatusUnauthorized || out["code"] != "unauthenticated" {
		t.Errorf("no token: %d %v", st, out)
	}
	other, _ := (&Signer{Key: []byte("other")}).MintJobToken("j", mainScope(), time.Hour)
	if st, _ := h.call(other, "CreateCacheEntry", map[string]any{"key": "k", "version": "v"}); st != http.StatusUnauthorized {
		t.Errorf("foreign token: %d", st)
	}
	tok := h.token(mainScope())
	if st, out := h.call(tok, "DeleteEverything", map[string]any{"key": "k", "version": "v"}); st != http.StatusNotFound || out["code"] != "bad_route" {
		t.Errorf("unknown method: %d %v", st, out)
	}
	for _, bad := range []map[string]any{
		{"key": "", "version": "v"},
		{"key": "a,b", "version": "v"},
		{"key": strings.Repeat("k", 513), "version": "v"},
		{"key": "k"},
		{"key": 5, "version": "v"},
	} {
		if st, out := h.call(tok, "CreateCacheEntry", bad); st != http.StatusBadRequest {
			t.Errorf("%v: %d %v", bad, st, out)
		}
	}

	h.save(tok, "k", []byte("data"))
	dl := h.ok(tok, "GetCacheEntryDownloadURL", map[string]any{"key": "k", "version": "v1"})["signedDownloadUrl"].(string)
	// A download URL can't be turned into an upload URL, or pointed at
	// another blob.
	if st := h.put(strings.Replace(dl, "sp=r", "sp=w", 1), []byte("x"), nil); st != http.StatusForbidden {
		t.Errorf("read URL used for write: %d", st)
	}
	if st := h.put(dl, []byte("x"), nil); st != http.StatusForbidden {
		t.Errorf("PUT with read URL: %d", st)
	}
	if st, _ := h.get(strings.Replace(dl, "/blobs/", "/blobs/0", 1), nil); st != http.StatusForbidden {
		t.Errorf("tampered blob id: %d", st)
	}
	if st, _ := h.get(h.http.URL+"/healthz", nil); st != http.StatusOK {
		t.Errorf("healthz: %d", st)
	}
}

func TestQuotaEvictionDeletesBlobs(t *testing.T) {
	h := newHarness(t, 10)
	tok := h.token(mainScope())
	h.save(tok, "a", []byte("123456"))
	h.save(tok, "b", []byte("123456")) // evicts a
	if _, _, ok := h.restore(tok, "a"); ok {
		t.Error("evicted entry still restorable")
	}
	if h.store.Len() != 1 {
		t.Errorf("evicted blob not deleted: %d blobs", h.store.Len())
	}
	c := h.ok(tok, "CreateCacheEntry", map[string]any{"key": "huge", "version": "v1"})
	if st := h.put(c["signedUploadUrl"].(string), bytes.Repeat([]byte("x"), 11), nil); st != http.StatusRequestEntityTooLarge {
		t.Errorf("upload over quota: %d", st)
	}
}

func TestPerRepoQuotaOverride(t *testing.T) {
	ix := &Index{QuotaBytes: 10, RepoQuota: func(r string) int64 {
		if r == repo {
			return 100
		}
		return 0
	}}
	must(t, ix.Put(mainScope(), Entry{Key: "a", Version: "v", SizeBytes: 50}))
	other := Scope{Repo: "acme/web", Ref: main, DefaultBranch: main, Trust: jobapi.Trusted}
	if err := ix.Put(other, Entry{Key: "a", Version: "v", SizeBytes: 50}); err == nil {
		t.Error("default quota not applied to other repo")
	}
	if DefaultQuotaBytes <= 10<<30 {
		t.Error("default quota must exceed GitHub's 10 GB")
	}
}

func TestStoreDownIsACacheMiss(t *testing.T) {
	h := newHarness(t, 0)
	tok := h.token(mainScope())
	h.save(tok, "k", []byte("data"))
	h.store.Fail = errors.New("object store unreachable")
	out := h.ok(tok, "GetCacheEntryDownloadURL", map[string]any{"key": "k", "version": "v1"})
	if out["ok"] != false {
		t.Errorf("lookup with store down = %v, want a miss", out)
	}
	h.store.Fail = nil
	if _, _, ok := h.restore(tok, "k"); !ok {
		t.Error("entry lost after store recovered")
	}
}

func TestPendingUploadsExpire(t *testing.T) {
	h := newHarness(t, 0)
	now := time.Now()
	h.srv.Now = func() time.Time { return now }
	tok := h.token(mainScope())
	c := h.ok(tok, "CreateCacheEntry", map[string]any{"key": "k", "version": "v1"})
	h.put(c["signedUploadUrl"].(string), []byte("abandoned"), nil)
	now = now.Add(defaultPendingTTL + time.Minute)
	// The next reservation sweeps the abandoned one, and the key is free.
	if out := h.ok(tok, "CreateCacheEntry", map[string]any{"key": "k", "version": "v1"}); out["ok"] != true {
		t.Fatalf("re-create after expiry: %v", out)
	}
	if h.store.Len() != 0 {
		t.Errorf("abandoned blob not deleted: %d", h.store.Len())
	}
}

func TestDirStore(t *testing.T) {
	d := DirStore{Root: t.TempDir()}
	ctx := t.Context()
	n, err := d.Put(ctx, "staging/x/01", strings.NewReader("hello"))
	if err != nil || n != 5 {
		t.Fatalf("put = %d %v", n, err)
	}
	b, err := d.Open(ctx, "staging/x/01")
	must(t, err)
	buf := make([]byte, 3)
	if _, err := b.ReadAt(buf, 1); err != nil || string(buf) != "ell" || b.Size() != 5 {
		t.Errorf("read = %q %v size %d", buf, err, b.Size())
	}
	b.Close()
	must(t, d.Delete(ctx, "staging/x/01"))
	if _, err := d.Open(ctx, "staging/x/01"); !errors.Is(err, ErrBlobNotFound) {
		t.Errorf("open deleted: %v", err)
	}
	if _, err := d.Put(ctx, "../escape", strings.NewReader("x")); err == nil {
		t.Error("path traversal accepted")
	}
}
