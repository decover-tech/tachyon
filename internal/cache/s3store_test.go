package cache

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testAccess = "AKIDEXAMPLE"
	testSecret = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	testRegion = "garage"
)

// fakeS3 is a path-style S3 server that verifies SigV4 on every request.
type fakeS3 struct {
	t      *testing.T
	bucket string

	mu      sync.Mutex
	objects map[string][]byte
	etags   map[string]string
	uploads map[string]map[int][]byte // upload id -> part number -> data
	upKey   map[string]string
	nextID  int
	aborted int
	calls   []string
	// inject, if set, may return a non-zero status to fail a request.
	inject func(r *http.Request) int
	// ignoreRange makes GET return 200 and the whole object.
	ignoreRange bool
	// completeErr makes CompleteMultipartUpload return 200 with an <Error>.
	completeErr bool
}

func newFakeS3(t *testing.T) (*fakeS3, *S3Store) {
	f := &fakeS3{t: t, bucket: "cache", objects: map[string][]byte{}, etags: map[string]string{},
		uploads: map[string]map[int][]byte{}, upKey: map[string]string{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, &S3Store{Endpoint: srv.URL, Bucket: "cache", Region: testRegion,
		Creds: S3Credentials{testAccess, testSecret}}
}

func (f *fakeS3) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func s3err(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<Error><Code>%s</Code><Message>%s</Message></Error>", code, msg)
}

// verifySig recomputes the signature from the request as received.
func (f *fakeS3) verifySig(r *http.Request, body []byte) error {
	auth := r.Header.Get("Authorization")
	const pfx = "AWS4-HMAC-SHA256 "
	if !strings.HasPrefix(auth, pfx) {
		return errors.New("no AWS4 authorization")
	}
	fields := map[string]string{}
	for _, p := range strings.Split(auth[len(pfx):], ", ") {
		k, v, _ := strings.Cut(p, "=")
		fields[k] = v
	}
	cred := strings.Split(fields["Credential"], "/")
	if len(cred) != 5 || cred[0] != testAccess || cred[2] != testRegion || cred[3] != "s3" || cred[4] != "aws4_request" {
		return fmt.Errorf("bad credential scope %q", fields["Credential"])
	}
	sum := sha256.Sum256(body)
	if got := r.Header.Get("X-Amz-Content-Sha256"); got != hex.EncodeToString(sum[:]) {
		return errors.New("payload hash does not match body")
	}
	stamp := r.Header.Get("X-Amz-Date")
	if !strings.HasPrefix(stamp, cred[1]) {
		return errors.New("date mismatch")
	}
	var canonHdrs strings.Builder
	for _, h := range strings.Split(fields["SignedHeaders"], ";") {
		v := r.Header.Get(h)
		if h == "host" {
			v = r.Host
		}
		canonHdrs.WriteString(h + ":" + strings.TrimSpace(v) + "\n")
	}
	if !strings.Contains(";"+fields["SignedHeaders"]+";", ";host;") ||
		!strings.Contains(";"+fields["SignedHeaders"]+";", ";x-amz-date;") ||
		!strings.Contains(";"+fields["SignedHeaders"]+";", ";x-amz-content-sha256;") {
		return errors.New("host, x-amz-date and x-amz-content-sha256 must be signed")
	}
	if r.Header.Get("Range") != "" && !strings.Contains(fields["SignedHeaders"], "range") {
		return errors.New("range must be signed")
	}
	q := r.URL.Query()
	var qs []string
	for k, vs := range q {
		for _, v := range vs {
			qs = append(qs, url.QueryEscape(k)+"="+url.QueryEscape(v))
		}
	}
	sort.Strings(qs)
	canonical := strings.Join([]string{r.Method, r.URL.EscapedPath(), strings.Join(qs, "&"),
		canonHdrs.String(), fields["SignedHeaders"], r.Header.Get("X-Amz-Content-Sha256")}, "\n")
	ch := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + stamp + "\n" + strings.Join(cred[1:], "/") + "\n" + hex.EncodeToString(ch[:])
	k := hmacSHA256([]byte("AWS4"+testSecret), cred[1])
	k = hmacSHA256(k, cred[2])
	k = hmacSHA256(k, cred[3])
	k = hmacSHA256(k, cred[4])
	if want := hex.EncodeToString(hmacSHA256(k, toSign)); want != fields["Signature"] {
		return errors.New("signature mismatch")
	}
	return nil
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if err := f.verifySig(r, body); err != nil {
		s3err(w, 403, "SignatureDoesNotMatch", err.Error())
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.RequestURI())
	if f.inject != nil {
		if st := f.inject(r); st != 0 {
			s3err(w, st, "InternalError", "injected")
			return
		}
	}
	pfx := "/" + f.bucket + "/"
	if r.URL.Path == "/"+f.bucket+"/" || r.URL.Path == "/"+f.bucket {
		w.WriteHeader(200)
		return
	}
	if !strings.HasPrefix(r.URL.Path, pfx) {
		s3err(w, 404, "NoSuchBucket", "no such bucket")
		return
	}
	key := strings.TrimPrefix(r.URL.Path, pfx)
	q := r.URL.Query()
	switch {
	case r.Method == "POST" && q.Has("uploads"):
		f.nextID++
		id := "up" + strconv.Itoa(f.nextID)
		f.uploads[id] = map[int][]byte{}
		f.upKey[id] = key
		fmt.Fprintf(w, "<InitiateMultipartUploadResult><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>", key, id)
	case r.Method == "PUT" && q.Has("partNumber"):
		parts, ok := f.uploads[q.Get("uploadId")]
		n, _ := strconv.Atoi(q.Get("partNumber"))
		if !ok || n < 1 {
			s3err(w, 404, "NoSuchUpload", "no such upload")
			return
		}
		parts[n] = body
		sum := sha256.Sum256(body)
		w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:8])+`"`)
	case r.Method == "POST" && q.Has("uploadId"):
		parts, ok := f.uploads[q.Get("uploadId")]
		if !ok {
			s3err(w, 404, "NoSuchUpload", "no such upload")
			return
		}
		if f.completeErr {
			w.WriteHeader(200)
			io.WriteString(w, "<Error><Code>InternalError</Code><Message>boom</Message></Error>")
			return
		}
		var req struct {
			Parts []struct {
				PartNumber int
				ETag       string
			} `xml:"Part"`
		}
		if err := xml.Unmarshal(body, &req); err != nil {
			s3err(w, 400, "MalformedXML", err.Error())
			return
		}
		var all []byte
		for i, p := range req.Parts {
			if p.PartNumber != i+1 {
				s3err(w, 400, "InvalidPartOrder", "parts out of order")
				return
			}
			sum := sha256.Sum256(parts[p.PartNumber])
			if p.ETag != `"`+hex.EncodeToString(sum[:8])+`"` {
				s3err(w, 400, "InvalidPart", "etag mismatch")
				return
			}
			if i < len(req.Parts)-1 && len(parts[p.PartNumber]) < minS3PartSize {
				s3err(w, 400, "EntityTooSmall", "part too small")
				return
			}
			all = append(all, parts[p.PartNumber]...)
		}
		f.store(key, all)
		delete(f.uploads, q.Get("uploadId"))
		io.WriteString(w, "<CompleteMultipartUploadResult><Key>"+key+"</Key></CompleteMultipartUploadResult>")
	case r.Method == "DELETE" && q.Has("uploadId"):
		delete(f.uploads, q.Get("uploadId"))
		f.aborted++
		w.WriteHeader(204)
	case r.Method == "PUT":
		f.store(key, body)
		w.Header().Set("ETag", f.etags[key])
	case r.Method == "HEAD" || r.Method == "GET":
		obj, ok := f.objects[key]
		if !ok {
			if r.Method == "HEAD" {
				w.WriteHeader(404)
			} else {
				s3err(w, 404, "NoSuchKey", "no such key")
			}
			return
		}
		w.Header().Set("ETag", f.etags[key])
		if im := r.Header.Get("If-Match"); im != "" && im != f.etags[key] {
			s3err(w, 412, "PreconditionFailed", "etag changed")
			return
		}
		rng := r.Header.Get("Range")
		if r.Method == "HEAD" || rng == "" || f.ignoreRange {
			w.Header().Set("Content-Length", strconv.Itoa(len(obj)))
			w.WriteHeader(200)
			if r.Method == "GET" {
				w.Write(obj)
			}
			return
		}
		var a, b int
		if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &a, &b); err != nil || a > b {
			s3err(w, 400, "InvalidArgument", "bad range")
			return
		}
		if a >= len(obj) {
			s3err(w, 416, "InvalidRange", "range not satisfiable")
			return
		}
		if b >= len(obj) {
			b = len(obj) - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(obj)))
		w.Header().Set("Content-Length", strconv.Itoa(b-a+1))
		w.WriteHeader(206)
		w.Write(obj[a : b+1])
	case r.Method == "DELETE":
		delete(f.objects, key) // S3 answers 204 even for missing keys
		w.WriteHeader(204)
	default:
		s3err(w, 405, "MethodNotAllowed", "nope")
	}
}

func (f *fakeS3) store(key string, b []byte) {
	f.objects[key] = append([]byte(nil), b...)
	sum := sha256.Sum256(b)
	f.etags[key] = `"` + hex.EncodeToString(sum[:8]) + `"`
}

func randBytes(t *testing.T, n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSigV4AWSVector(t *testing.T) {
	// "GET Object" example from the AWS SigV4 documentation (header auth).
	req, _ := http.NewRequest("GET", "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	req.Header.Set("Range", "bytes=0-9")
	emptyHash := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	signV4(req, emptyHash, S3Credentials{"AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"},
		"us-east-1", time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, " +
		"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestS3PutOpenRoundTrip(t *testing.T) {
	f, s := newFakeS3(t)
	ctx := context.Background()
	data := randBytes(t, 1000)
	n, err := s.Put(ctx, "ab/cd.blob", bytes.NewReader(data))
	if err != nil || n != 1000 {
		t.Fatalf("Put = %d, %v", n, err)
	}
	if !bytes.Equal(f.objects["ab/cd.blob"], data) {
		t.Fatal("stored bytes differ")
	}
	if calls := f.callList(); len(calls) != 1 || !strings.HasPrefix(calls[0], "PUT ") {
		t.Fatalf("small blob should be one PUT, calls = %v", calls)
	}
	b, err := s.Open(ctx, "ab/cd.blob")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if b.Size() != 1000 {
		t.Fatalf("Size = %d", b.Size())
	}
	got, err := io.ReadAll(io.NewSectionReader(b, 0, b.Size()))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read all: %v", err)
	}
}

func TestS3PutEmpty(t *testing.T) {
	f, s := newFakeS3(t)
	n, err := s.Put(context.Background(), "empty", strings.NewReader(""))
	if err != nil || n != 0 {
		t.Fatalf("Put = %d, %v", n, err)
	}
	if v, ok := f.objects["empty"]; !ok || len(v) != 0 {
		t.Fatal("empty object not stored")
	}
	b, err := s.Open(context.Background(), "empty")
	if err != nil || b.Size() != 0 {
		t.Fatalf("Open: %v", err)
	}
	if n, err := b.ReadAt(make([]byte, 4), 0); n != 0 || err != io.EOF {
		t.Fatalf("ReadAt on empty = %d, %v", n, err)
	}
}

func TestS3PrefixAndPathStyle(t *testing.T) {
	f, s := newFakeS3(t)
	s.Prefix = "cache/v1/"
	if _, err := s.Put(context.Background(), "x", strings.NewReader("hi")); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.objects["cache/v1/x"]; !ok {
		t.Fatalf("objects = %v", f.objects)
	}
}

func TestS3MultipartExactBoundaries(t *testing.T) {
	const ps = minS3PartSize
	for _, tc := range []struct {
		name      string
		size      int
		wantParts int // 0 = single PutObject
	}{
		{"exactly one part is a single put", ps, 0},
		{"one byte over", ps + 1, 2},
		{"two exact parts", 2 * ps, 2},
		{"two and a bit", 2*ps + 123, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, s := newFakeS3(t)
			s.PartSize = ps
			data := randBytes(t, tc.size)
			// iotest-style reader that returns short reads.
			n, err := s.Put(context.Background(), "big", &chunkReader{data: data, max: 100_000})
			if err != nil || n != int64(tc.size) {
				t.Fatalf("Put = %d, %v", n, err)
			}
			if !bytes.Equal(f.objects["big"], data) {
				t.Fatal("stored bytes differ")
			}
			parts := 0
			for _, c := range f.callList() {
				if strings.Contains(c, "partNumber=") {
					parts++
				}
			}
			if parts != tc.wantParts {
				t.Fatalf("parts = %d, want %d (calls %v)", parts, tc.wantParts, f.callList())
			}
			if len(f.uploads) != 0 {
				t.Fatal("upload left open")
			}
		})
	}
}

type chunkReader struct {
	data []byte
	max  int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	n := min(len(p), c.max, len(c.data))
	copy(p, c.data[:n])
	c.data = c.data[n:]
	return n, nil
}

func TestS3PartSizeClampedToMinimum(t *testing.T) {
	s := &S3Store{PartSize: 1}
	if s.partSize() != minS3PartSize {
		t.Fatalf("partSize = %d", s.partSize())
	}
	if (&S3Store{}).partSize() != DefaultS3PartSize {
		t.Fatal("default part size")
	}
}

func TestS3MultipartAbortOnPartFailure(t *testing.T) {
	f, s := newFakeS3(t)
	s.PartSize = minS3PartSize
	f.inject = func(r *http.Request) int {
		if r.URL.Query().Get("partNumber") == "2" {
			return 500
		}
		return 0
	}
	_, err := s.Put(context.Background(), "big", bytes.NewReader(randBytes(t, 3*minS3PartSize)))
	var se *S3Error
	if !errors.As(err, &se) || se.Status != 500 || se.Op != "UploadPart" || se.Code != "InternalError" {
		t.Fatalf("err = %v", err)
	}
	if f.aborted != 1 || len(f.uploads) != 0 {
		t.Fatalf("aborted=%d open=%d", f.aborted, len(f.uploads))
	}
	if _, ok := f.objects["big"]; ok {
		t.Fatal("object must not exist")
	}
}

func TestS3MultipartAbortOnReaderError(t *testing.T) {
	f, s := newFakeS3(t)
	s.PartSize = minS3PartSize
	boom := errors.New("client went away")
	r := io.MultiReader(bytes.NewReader(randBytes(t, minS3PartSize+10)), errReader{boom})
	_, err := s.Put(context.Background(), "big", r)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if f.aborted != 1 {
		t.Fatalf("aborted = %d", f.aborted)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func TestS3MultipartCompleteReturns200WithError(t *testing.T) {
	f, s := newFakeS3(t)
	s.PartSize = minS3PartSize
	f.completeErr = true
	_, err := s.Put(context.Background(), "big", bytes.NewReader(randBytes(t, minS3PartSize+1)))
	var se *S3Error
	if !errors.As(err, &se) || se.Op != "CompleteMultipartUpload" || se.Code != "InternalError" {
		t.Fatalf("err = %v", err)
	}
	if f.aborted != 1 {
		t.Fatalf("aborted = %d", f.aborted)
	}
}

func TestS3PutErrors(t *testing.T) {
	f, s := newFakeS3(t)
	ctx := context.Background()
	if _, err := s.Put(ctx, "../evil", strings.NewReader("x")); err == nil {
		t.Fatal("invalid id accepted")
	}
	f.inject = func(*http.Request) int { return 503 }
	_, err := s.Put(ctx, "k", strings.NewReader("x"))
	var se *S3Error
	if !errors.As(err, &se) || se.Status != 503 || se.Op != "PutObject" {
		t.Fatalf("err = %v", err)
	}
	// Reader error on the first part.
	f.inject = nil
	boom := errors.New("boom")
	if _, err := s.Put(ctx, "k", errReader{boom}); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if len(f.objects) != 0 {
		t.Fatal("nothing should be stored")
	}
}

func TestS3ReadAtRanges(t *testing.T) {
	_, s := newFakeS3(t)
	ctx := context.Background()
	data := randBytes(t, 10_000)
	if _, err := s.Put(ctx, "r", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	b, err := s.Open(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		off, n  int64
		wantN   int
		wantEOF bool
	}{
		{0, 10, 10, false},
		{9990, 10, 10, false}, // ends exactly at EOF: no error
		{5000, 1, 1, false},
		{9995, 100, 5, true}, // runs past EOF: short read with io.EOF
		{10_000, 1, 0, true},
		{20_000, 1, 0, true},
	} {
		p := make([]byte, tc.n)
		n, err := b.ReadAt(p, tc.off)
		if n != tc.wantN || (err == io.EOF) != tc.wantEOF || (err != nil && err != io.EOF) {
			t.Errorf("ReadAt(%d,%d) = %d, %v", tc.off, tc.n, n, err)
		}
		if n > 0 && !bytes.Equal(p[:n], data[tc.off:tc.off+int64(n)]) {
			t.Errorf("ReadAt(%d,%d) wrong bytes", tc.off, tc.n)
		}
	}
	if n, err := b.ReadAt(nil, 5); n != 0 || err != nil {
		t.Fatalf("zero-length ReadAt = %d, %v", n, err)
	}
	if _, err := b.ReadAt(make([]byte, 1), -1); err == nil {
		t.Fatal("negative offset accepted")
	}
	// io.SectionReader / io.Copy style access works.
	got := make([]byte, 100)
	if _, err := io.ReadFull(io.NewSectionReader(b, 4000, 100), got); err != nil || !bytes.Equal(got, data[4000:4100]) {
		t.Fatalf("section read: %v", err)
	}
}

func TestS3ReadAtSendsRangeAndIfMatch(t *testing.T) {
	f, s := newFakeS3(t)
	ctx := context.Background()
	if _, err := s.Put(ctx, "r", strings.NewReader("0123456789")); err != nil {
		t.Fatal(err)
	}
	b, _ := s.Open(ctx, "r")
	var gotRange, gotMatch string
	f.inject = func(r *http.Request) int {
		if r.Method == "GET" {
			gotRange, gotMatch = r.Header.Get("Range"), r.Header.Get("If-Match")
		}
		return 0
	}
	p := make([]byte, 3)
	if _, err := b.ReadAt(p, 4); err != nil || string(p) != "456" {
		t.Fatalf("ReadAt = %q, %v", p, err)
	}
	if gotRange != "bytes=4-6" {
		t.Fatalf("Range = %q", gotRange)
	}
	if gotMatch == "" {
		t.Fatal("If-Match not sent")
	}
}

func TestS3ReadAtObjectReplaced(t *testing.T) {
	f, s := newFakeS3(t)
	ctx := context.Background()
	s.Put(ctx, "r", strings.NewReader("aaaaaaaaaa"))
	b, _ := s.Open(ctx, "r")
	s.Put(ctx, "r", strings.NewReader("bbbbbbbbbbbb")) // replaced after Open
	_, err := b.ReadAt(make([]byte, 4), 0)
	var se *S3Error
	if !errors.As(err, &se) || se.Status != 412 {
		t.Fatalf("err = %v, want 412 (no torn reads across versions)", err)
	}
	_ = f
}

func TestS3ReadAtStoreIgnoresRange(t *testing.T) {
	f, s := newFakeS3(t)
	ctx := context.Background()
	s.Put(ctx, "r", strings.NewReader("0123456789"))
	b, _ := s.Open(ctx, "r")
	f.ignoreRange = true
	n, err := b.ReadAt(make([]byte, 3), 4)
	var se *S3Error
	if n != 0 || !errors.As(err, &se) || se.Status != 200 {
		t.Fatalf("ReadAt = %d, %v", n, err)
	}
}

func TestS3ReadAtDeletedAndTruncated(t *testing.T) {
	f, s := newFakeS3(t)
	ctx := context.Background()
	s.Put(ctx, "r", strings.NewReader("0123456789"))
	b, _ := s.Open(ctx, "r")
	f.mu.Lock()
	delete(f.etags, "r")
	delete(f.objects, "r")
	f.mu.Unlock()
	if _, err := b.ReadAt(make([]byte, 3), 0); !errors.Is(err, ErrBlobNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestS3ReadAtShortBodyIsUnexpectedEOF(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.Header().Set("Content-Length", "100")
			return
		}
		w.Header().Set("Content-Length", "10")
		w.WriteHeader(206)
		w.Write([]byte("short"))
	}))
	defer srv.Close()
	s := &S3Store{Endpoint: srv.URL, Bucket: "b", Region: "r", Creds: S3Credentials{"a", "b"}}
	b, err := s.Open(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	n, err := b.ReadAt(make([]byte, 10), 0)
	if n != 5 || err != io.ErrUnexpectedEOF {
		t.Fatalf("ReadAt = %d, %v", n, err)
	}
}

func TestS3OpenMissingAndErrors(t *testing.T) {
	f, s := newFakeS3(t)
	ctx := context.Background()
	if _, err := s.Open(ctx, "nope"); !errors.Is(err, ErrBlobNotFound) {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.Open(ctx, "bad id"); err == nil || errors.Is(err, ErrBlobNotFound) {
		t.Fatalf("err = %v", err)
	}
	f.inject = func(*http.Request) int { return 500 }
	_, err := s.Open(ctx, "k")
	var se *S3Error
	if !errors.As(err, &se) || se.Status != 500 || errors.Is(err, ErrBlobNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestS3Delete(t *testing.T) {
	f, s := newFakeS3(t)
	ctx := context.Background()
	s.Put(ctx, "d", strings.NewReader("x"))
	if err := s.Delete(ctx, "d"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.objects["d"]; ok {
		t.Fatal("still stored")
	}
	if err := s.Delete(ctx, "d"); !errors.Is(err, ErrBlobNotFound) {
		t.Fatalf("second delete = %v", err)
	}
	if err := s.Delete(ctx, "never"); !errors.Is(err, ErrBlobNotFound) {
		t.Fatalf("err = %v", err)
	}
	s.Put(ctx, "e", strings.NewReader("x"))
	f.inject = func(r *http.Request) int {
		if r.Method == "DELETE" {
			return 500
		}
		return 0
	}
	var se *S3Error
	if err := s.Delete(ctx, "e"); !errors.As(err, &se) || se.Status != 500 {
		t.Fatalf("err = %v", err)
	}
}

func TestS3BadCredentialsRejected(t *testing.T) {
	_, s := newFakeS3(t)
	s.Creds.SecretKey = "wrong"
	_, err := s.Put(context.Background(), "k", strings.NewReader("x"))
	var se *S3Error
	if !errors.As(err, &se) || se.Status != 403 || se.Code != "SignatureDoesNotMatch" {
		t.Fatalf("err = %v", err)
	}
	s.Creds = S3Credentials{"OTHER", testSecret}
	if _, err := s.Open(context.Background(), "k"); err == nil {
		t.Fatal("wrong access key accepted")
	}
}

func TestS3ContextCancel(t *testing.T) {
	_, s := newFakeS3(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Put(ctx, "k", strings.NewReader("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestS3ConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	s := &S3Store{Endpoint: url, Bucket: "b", Region: "r", Creds: S3Credentials{"a", "s3cr3t"}}
	_, err := s.Put(context.Background(), "k", strings.NewReader("x"))
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "s3cr3t") {
		t.Fatal("secret leaked into error")
	}
}

func TestS3EndpointWithPathAndCheck(t *testing.T) {
	f, s := newFakeS3(t)
	if err := s.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.Bucket = "missing"
	if err := s.Check(context.Background()); err == nil {
		t.Fatal("missing bucket accepted")
	}
	_ = f
}

func TestS3ConcurrentReadAt(t *testing.T) {
	_, s := newFakeS3(t)
	ctx := context.Background()
	data := randBytes(t, 50_000)
	s.Put(ctx, "c", bytes.NewReader(data))
	b, _ := s.Open(ctx, "c")
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := make([]byte, 1000)
			off := int64(i * 1000)
			if _, err := b.ReadAt(p, off); err != nil || !bytes.Equal(p, data[off:off+1000]) {
				t.Errorf("reader %d: %v", i, err)
			}
		}()
	}
	wg.Wait()
}

func TestS3CredentialsNeverPrint(t *testing.T) {
	c := S3Credentials{"AK", "supersecret"}
	for _, s := range []string{fmt.Sprint(c), fmt.Sprintf("%v %+v %#v", c, c, c), fmt.Sprint(&S3Store{Creds: c})} {
		if strings.Contains(s, "supersecret") {
			t.Fatalf("secret printed: %s", s)
		}
	}
}

func TestS3Validate(t *testing.T) {
	ok := S3Store{Endpoint: "http://h:1", Bucket: "b", Region: "r", Creds: S3Credentials{"a", "b"}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*S3Store){
		"endpoint": func(s *S3Store) { s.Endpoint = "h:1" },
		"scheme":   func(s *S3Store) { s.Endpoint = "ftp://h" },
		"bucket":   func(s *S3Store) { s.Bucket = "" },
		"region":   func(s *S3Store) { s.Region = "" },
		"access":   func(s *S3Store) { s.Creds.AccessKey = "" },
		"secret":   func(s *S3Store) { s.Creds.SecretKey = "" },
	} {
		s := ok
		mut(&s)
		if s.Validate() == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

// Concatenating multi-reader puts (as the server's commit does) work.
func TestS3PutMultiReader(t *testing.T) {
	_, s := newFakeS3(t)
	var _ BlobStore = s
	ctx := context.Background()
	if _, err := s.Put(ctx, "staged/1", strings.NewReader("hello ")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, "final", io.MultiReader(strings.NewReader("hello "), strings.NewReader("world"))); err != nil {
		t.Fatal(err)
	}
	b, err := s.Open(ctx, "final")
	if err != nil || b.Size() != 11 {
		t.Fatalf("Open: %v", err)
	}
}
