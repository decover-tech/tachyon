package cache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/decover-tech/tachyon/internal/jobapi"
)

// TwirpPrefix is the path of the cache service the actions/toolkit cache
// client calls (cache service v2, as of mid-2026): it POSTs JSON to
// <ACTIONS_RESULTS_URL>/twirp/github.actions.results.api.v1.CacheService/<Method>
// and moves the archive itself through the signed blob URL it gets back.
const TwirpPrefix = "/twirp/github.actions.results.api.v1.CacheService/"

const (
	maxKeyLen         = 512
	maxRestoreKeys    = 10
	maxTwirpBody      = 1 << 20
	defaultURLTTL     = time.Hour
	defaultPendingTTL = 6 * time.Hour
)

// Server is the regional Actions cache service: the Twirp API that
// actions/cache and the setup-* actions speak, plus a blob endpoint that
// accepts the Azure Block Blob subset the toolkit uploads with (Put Blob, Put
// Block, Put Block List) and serves ranged downloads.
//
// Callers authenticate with a job token from Signer.MintJobToken, which
// carries the job's Scope; the scoping and quota rules are Index's.
type Server struct {
	Index  *Index
	Store  BlobStore
	Signer *Signer
	// PublicURL is the base URL VMs reach the service at, without a
	// trailing slash, e.g. http://cache.r1.tachyon.internal.
	PublicURL string
	// URLTTL bounds signed blob URLs; PendingTTL bounds how long a reserved
	// upload may stay unfinalized.
	URLTTL, PendingTTL time.Duration
	Log                *slog.Logger
	Now                func() time.Time

	mu      sync.Mutex
	pending map[string]*upload // blob id -> reserved upload
	byKey   map[string]string  // repo/ref/key/version -> blob id
	evicted []string           // blob ids evicted from Index, to delete
	nextID  int64
}

type upload struct {
	scopeKey string
	scope    Scope
	key      string
	version  string
	created  time.Time
	blocks   map[string]string // Azure block id -> staged blob id
}

// NewServer wires the index's eviction to blob deletion.
func NewServer(ix *Index, store BlobStore, signer *Signer, publicURL string) *Server {
	s := &Server{Index: ix, Store: store, Signer: signer, PublicURL: strings.TrimRight(publicURL, "/")}
	prev := ix.OnEvict
	ix.OnEvict = func(e Entry) {
		if prev != nil {
			prev(e)
		}
		// Called with the index locked: only queue here, delete later.
		s.mu.Lock()
		s.evicted = append(s.evicted, e.BlobID)
		s.mu.Unlock()
	}
	return s
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Server) urlTTL() time.Duration {
	if s.URLTTL > 0 {
		return s.URLTTL
	}
	return defaultURLTTL
}

func (s *Server) pendingTTL() time.Duration {
	if s.PendingTTL > 0 {
		return s.PendingTTL
	}
	return defaultPendingTTL
}

// Handler serves the Twirp API, the blob endpoint and /healthz.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+TwirpPrefix+"{method}", s.serveTwirp)
	mux.HandleFunc("PUT /blobs/{id}", s.servePut)
	mux.HandleFunc("GET /blobs/{id}", s.serveGet)
	mux.HandleFunc("HEAD /blobs/{id}", s.serveGet)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	return mux
}

// --- Twirp ---

type twirpError struct {
	status int
	Code   string `json:"code"`
	Msg    string `json:"msg"`
}

func (e *twirpError) Error() string { return e.Code + ": " + e.Msg }

func twerr(status int, code, format string, args ...any) *twirpError {
	return &twirpError{status: status, Code: code, Msg: fmt.Sprintf(format, args...)}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// request is a decoded Twirp JSON request. The toolkit's generated client
// sends proto field names (restore_keys); canonical proto3 JSON uses
// lowerCamelCase (restoreKeys). Both are accepted, and int64 fields may be
// JSON strings or numbers.
type request map[string]json.RawMessage

func (r request) raw(names ...string) json.RawMessage {
	for _, n := range names {
		if v, ok := r[n]; ok && string(v) != "null" {
			return v
		}
	}
	return nil
}

func (r request) str(names ...string) (string, error) {
	var s string
	if v := r.raw(names...); v != nil {
		if err := json.Unmarshal(v, &s); err != nil {
			return "", twerr(http.StatusBadRequest, "malformed", "%s must be a string", names[0])
		}
	}
	return s, nil
}

func (r request) strs(names ...string) ([]string, error) {
	var s []string
	if v := r.raw(names...); v != nil {
		if err := json.Unmarshal(v, &s); err != nil {
			return nil, twerr(http.StatusBadRequest, "malformed", "%s must be a list of strings", names[0])
		}
	}
	return s, nil
}

func (r request) int64(names ...string) (int64, error) {
	v := r.raw(names...)
	if v == nil {
		return 0, nil
	}
	n, err := strconv.ParseInt(strings.Trim(string(v), `"`), 10, 64)
	if err != nil {
		return 0, twerr(http.StatusBadRequest, "malformed", "%s must be an integer", names[0])
	}
	return n, nil
}

func (s *Server) serveTwirp(w http.ResponseWriter, r *http.Request) {
	resp, err := s.twirp(r)
	if err != nil {
		var te *twirpError
		if !errors.As(err, &te) {
			s.log().Error("cache twirp", "method", r.PathValue("method"), "err", err)
			te = twerr(http.StatusInternalServerError, "internal", "internal error")
		}
		writeJSON(w, te.status, te)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) twirp(r *http.Request) (any, error) {
	claims, err := s.authenticate(r)
	if err != nil {
		return nil, err
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return nil, twerr(http.StatusNotFound, "bad_route", "only application/json is supported, got %q", ct)
	}
	var req request
	if err := json.NewDecoder(io.LimitReader(r.Body, maxTwirpBody)).Decode(&req); err != nil {
		return nil, twerr(http.StatusBadRequest, "malformed", "decode request: %v", err)
	}
	key, err := req.str("key")
	if err != nil {
		return nil, err
	}
	version, err := req.str("version")
	if err != nil {
		return nil, err
	}
	if err := validateKey(key, version); err != nil {
		return nil, err
	}
	scope := claims.Scope()
	ctx := r.Context()
	switch r.PathValue("method") {
	case "CreateCacheEntry":
		return s.create(scope, key, version)
	case "FinalizeCacheEntryUpload":
		size, err := req.int64("size_bytes", "sizeBytes")
		if err != nil {
			return nil, err
		}
		return s.finalize(ctx, scope, key, version, size)
	case "GetCacheEntryDownloadURL":
		restore, err := req.strs("restore_keys", "restoreKeys")
		if err != nil {
			return nil, err
		}
		if len(restore) > maxRestoreKeys {
			return nil, twerr(http.StatusBadRequest, "invalid_argument", "at most %d restore keys", maxRestoreKeys)
		}
		return s.downloadURL(ctx, scope, key, version, restore)
	}
	return nil, twerr(http.StatusNotFound, "bad_route", "no method %q", r.PathValue("method"))
}

func (s *Server) authenticate(r *http.Request) (Claims, error) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		return Claims{}, twerr(http.StatusUnauthorized, "unauthenticated", "missing bearer token")
	}
	c, err := s.Signer.VerifyJobToken(tok)
	if err != nil {
		return Claims{}, twerr(http.StatusUnauthorized, "unauthenticated", "%v", err)
	}
	return c, nil
}

func validateKey(key, version string) error {
	switch {
	case key == "":
		return twerr(http.StatusBadRequest, "invalid_argument", "key required")
	case len(key) > maxKeyLen:
		return twerr(http.StatusBadRequest, "invalid_argument", "key longer than %d characters", maxKeyLen)
	case strings.Contains(key, ","):
		return twerr(http.StatusBadRequest, "invalid_argument", "key cannot contain commas")
	case version == "":
		return twerr(http.StatusBadRequest, "invalid_argument", "version required")
	}
	return nil
}

func scopeKey(sc Scope, key, version string) string {
	return sc.Repo + "\x00" + sc.Ref + "\x00" + key + "\x00" + version
}

type createResponse struct {
	OK              bool   `json:"ok"`
	SignedUploadURL string `json:"signedUploadUrl,omitempty"`
	Message         string `json:"message,omitempty"`
}

func (s *Server) create(sc Scope, key, version string) (any, error) {
	// Refusals are ok:false with a message, not errors: the toolkit logs a
	// warning and the job carries on, as it does on GitHub.
	if sc.Trust == jobapi.UntrustedFork {
		return createResponse{Message: ErrForkWrite.Error()}, nil
	}
	if s.Index.Has(sc, version, key) {
		return createResponse{Message: fmt.Sprintf("cache entry %q already exists", key)}, nil
	}
	s.expirePending()
	sk := scopeKey(sc, key, version)
	s.mu.Lock()
	if s.pending == nil {
		s.pending, s.byKey = map[string]*upload{}, map[string]string{}
	}
	if _, busy := s.byKey[sk]; busy {
		s.mu.Unlock()
		return createResponse{Message: fmt.Sprintf("another job is already saving cache entry %q", key)}, nil
	}
	id := newBlobID()
	s.pending[id] = &upload{scopeKey: sk, scope: sc, key: key, version: version, created: s.now(), blocks: map[string]string{}}
	s.byKey[sk] = id
	s.mu.Unlock()
	return createResponse{OK: true, SignedUploadURL: s.blobURL(id, permWrite)}, nil
}

type finalizeResponse struct {
	OK      bool   `json:"ok"`
	EntryID string `json:"entryId,omitempty"` // int64, as a JSON string
	Message string `json:"message,omitempty"`
}

func (s *Server) finalize(ctx context.Context, sc Scope, key, version string, size int64) (any, error) {
	sk := scopeKey(sc, key, version)
	s.mu.Lock()
	id, ok := s.byKey[sk]
	if ok {
		delete(s.byKey, sk)
		delete(s.pending, id)
	}
	s.mu.Unlock()
	if !ok {
		return finalizeResponse{Message: fmt.Sprintf("no upload reserved for cache entry %q", key)}, nil
	}
	blob, err := s.Store.Open(ctx, id)
	if err != nil {
		if !errors.Is(err, ErrBlobNotFound) {
			s.log().Warn("cache finalize: open blob", "blob", id, "err", err)
		}
		return finalizeResponse{Message: "cache archive was not uploaded"}, nil
	}
	actual := blob.Size()
	blob.Close()
	fail := func(msg string) (any, error) {
		s.deleteBlob(ctx, id)
		return finalizeResponse{Message: msg}, nil
	}
	if size > 0 && size != actual {
		return fail(fmt.Sprintf("size_bytes %d does not match uploaded %d bytes", size, actual))
	}
	if err := s.Index.Put(sc, Entry{Key: key, Version: version, SizeBytes: actual, BlobID: id}); err != nil {
		return fail(err.Error())
	}
	s.drainEvicted(ctx)
	s.mu.Lock()
	s.nextID++
	n := s.nextID
	s.mu.Unlock()
	return finalizeResponse{OK: true, EntryID: strconv.FormatInt(n, 10)}, nil
}

type downloadResponse struct {
	OK                bool   `json:"ok"`
	SignedDownloadURL string `json:"signedDownloadUrl,omitempty"`
	MatchedKey        string `json:"matchedKey,omitempty"`
}

func (s *Server) downloadURL(ctx context.Context, sc Scope, key, version string, restore []string) (any, error) {
	e, ok := s.Index.Lookup(sc, version, key, restore)
	if !ok {
		return downloadResponse{}, nil
	}
	// If the object store can't serve the blob, report a miss rather than
	// an error: the job rebuilds instead of failing.
	blob, err := s.Store.Open(ctx, e.BlobID)
	if err != nil {
		s.log().Warn("cache lookup: blob unavailable, reporting miss", "repo", sc.Repo, "key", e.Key, "err", err)
		return downloadResponse{}, nil
	}
	blob.Close()
	return downloadResponse{OK: true, SignedDownloadURL: s.blobURL(e.BlobID, permRead), MatchedKey: e.Key}, nil
}

func (s *Server) blobURL(id, perm string) string {
	return s.PublicURL + "/blobs/" + id + "?" + s.Signer.blobQuery(id, perm, s.urlTTL())
}

func newBlobID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func (s *Server) deleteBlob(ctx context.Context, id string) {
	if err := s.Store.Delete(ctx, id); err != nil && !errors.Is(err, ErrBlobNotFound) {
		s.log().Warn("cache: delete blob", "blob", id, "err", err)
	}
}

func (s *Server) drainEvicted(ctx context.Context) {
	s.mu.Lock()
	ids := s.evicted
	s.evicted = nil
	s.mu.Unlock()
	for _, id := range ids {
		s.deleteBlob(ctx, id)
	}
}

// expirePending drops uploads reserved but never finalized, and their bytes.
func (s *Server) expirePending() {
	cutoff := s.now().Add(-s.pendingTTL())
	var stale []*upload
	var ids []string
	s.mu.Lock()
	for id, u := range s.pending {
		if u.created.Before(cutoff) {
			stale = append(stale, u)
			ids = append(ids, id)
			delete(s.pending, id)
			delete(s.byKey, u.scopeKey)
		}
	}
	s.mu.Unlock()
	ctx := context.Background()
	for i, u := range stale {
		for _, staged := range u.blocks {
			s.deleteBlob(ctx, staged)
		}
		s.deleteBlob(ctx, ids[i])
	}
}

// --- Blob endpoint (Azure Block Blob subset) ---

func (s *Server) checkBlobURL(r *http.Request, perm string) (string, int, error) {
	id := r.PathValue("id")
	q := r.URL.Query()
	if !validBlobID(id) || q.Get("sp") != perm {
		return "", http.StatusForbidden, ErrBadToken
	}
	if err := s.Signer.verifyBlob(id, perm, q.Get("se"), q.Get("sig")); err != nil {
		return "", http.StatusForbidden, err
	}
	return id, 0, nil
}

func setBlobHeaders(w http.ResponseWriter, t time.Time) {
	h := w.Header()
	h.Set("x-ms-version", "2023-11-03")
	h.Set("x-ms-request-id", newBlobID())
	h.Set("x-ms-request-server-encrypted", "false")
	h.Set("Last-Modified", t.UTC().Format(http.TimeFormat))
	h.Set("ETag", `"`+strconv.FormatInt(t.UnixNano(), 16)+`"`)
}

func (s *Server) servePut(w http.ResponseWriter, r *http.Request) {
	id, status, err := s.checkBlobURL(r, permWrite)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	s.mu.Lock()
	u := s.pending[id]
	s.mu.Unlock()
	if u == nil {
		http.Error(w, "upload is not open", http.StatusConflict)
		return
	}
	limit := s.Index.quota(u.scope.Repo)
	if limit <= 0 {
		limit = DefaultQuotaBytes
	}
	body := http.MaxBytesReader(w, r.Body, limit)
	ctx := r.Context()
	q := r.URL.Query()
	switch q.Get("comp") {
	case "":
		_, err = s.Store.Put(ctx, id, body)
	case "block":
		err = s.putBlock(ctx, u, id, q.Get("blockid"), body)
	case "blocklist":
		err = s.putBlockList(ctx, u, id, body, limit)
	default:
		http.Error(w, "unsupported comp", http.StatusBadRequest)
		return
	}
	if err != nil {
		var mbe *http.MaxBytesError
		switch {
		case errors.As(err, &mbe):
			http.Error(w, "upload exceeds repo cache quota", http.StatusRequestEntityTooLarge)
		case errors.Is(err, errBadBlock):
			http.Error(w, err.Error(), http.StatusBadRequest)
		default:
			s.log().Warn("cache blob put", "blob", id, "err", err)
			http.Error(w, "store error", http.StatusInternalServerError)
		}
		return
	}
	setBlobHeaders(w, s.now())
	w.WriteHeader(http.StatusCreated)
}

var errBadBlock = errors.New("bad block")

func (s *Server) putBlock(ctx context.Context, u *upload, id, blockID string, body io.Reader) error {
	if blockID == "" || len(blockID) > 128 {
		return fmt.Errorf("%w: blockid required", errBadBlock)
	}
	staged := "staging/" + id + "/" + hex.EncodeToString([]byte(blockID))
	if _, err := s.Store.Put(ctx, staged, body); err != nil {
		return err
	}
	s.mu.Lock()
	u.blocks[blockID] = staged
	s.mu.Unlock()
	return nil
}

// parseBlockList reads <BlockList><Latest|Committed|Uncommitted>id</...>...
// in order.
func parseBlockList(r io.Reader) ([]string, error) {
	dec := xml.NewDecoder(r)
	var ids []string
	depth := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errBadBlock, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth == 1 && t.Name.Local != "BlockList" {
				return nil, fmt.Errorf("%w: want BlockList, got %s", errBadBlock, t.Name.Local)
			}
			if depth == 2 {
				var v string
				if err := dec.DecodeElement(&v, &t); err != nil {
					return nil, fmt.Errorf("%w: %v", errBadBlock, err)
				}
				ids = append(ids, strings.TrimSpace(v))
				depth--
			}
		case xml.EndElement:
			depth--
		}
	}
	return ids, nil
}

func (s *Server) putBlockList(ctx context.Context, u *upload, id string, body io.Reader, limit int64) error {
	ids, err := parseBlockList(body)
	if err != nil {
		return err
	}
	s.mu.Lock()
	staged := make([]string, len(ids))
	for i, b := range ids {
		staged[i] = u.blocks[b]
	}
	s.mu.Unlock()
	var parts []io.Reader
	var total int64
	for i, st := range staged {
		if st == "" {
			return fmt.Errorf("%w: block %q was not staged", errBadBlock, ids[i])
		}
		b, err := s.Store.Open(ctx, st)
		if err != nil {
			return err
		}
		defer b.Close()
		total += b.Size()
		parts = append(parts, io.NewSectionReader(b, 0, b.Size()))
	}
	if total > limit {
		return &http.MaxBytesError{Limit: limit}
	}
	if _, err := s.Store.Put(ctx, id, io.MultiReader(parts...)); err != nil {
		return err
	}
	s.mu.Lock()
	blocks := u.blocks
	u.blocks = map[string]string{}
	s.mu.Unlock()
	for _, st := range blocks {
		s.deleteBlob(ctx, st)
	}
	return nil
}

func (s *Server) serveGet(w http.ResponseWriter, r *http.Request) {
	id, status, err := s.checkBlobURL(r, permRead)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	blob, err := s.Store.Open(r.Context(), id)
	if errors.Is(err, ErrBlobNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		s.log().Warn("cache blob get", "blob", id, "err", err)
		http.Error(w, "store error", http.StatusServiceUnavailable)
		return
	}
	defer blob.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("x-ms-blob-type", "BlockBlob")
	http.ServeContent(w, r, "", time.Time{}, io.NewSectionReader(blob, 0, blob.Size()))
}
