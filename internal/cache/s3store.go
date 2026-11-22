package cache

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Default and minimum multipart part sizes. S3 requires every part but the
// last to be at least 5 MiB and allows at most 10000 parts, so 16 MiB parts
// cap a blob at 160 GiB.
const (
	DefaultS3PartSize = 16 << 20
	minS3PartSize     = 5 << 20
	maxS3Parts        = 10000
)

// S3Credentials is an access key pair. It never prints its secret.
type S3Credentials struct {
	AccessKey string
	SecretKey string
}

func (c S3Credentials) String() string   { return "S3Credentials{[REDACTED]}" }
func (c S3Credentials) GoString() string { return c.String() }

// S3Error is a non-success response from the store. It carries only the
// status, S3 error code and message, never request headers.
type S3Error struct {
	Op      string
	Status  int
	Code    string
	Message string
}

func (e *S3Error) Error() string {
	s := fmt.Sprintf("s3 %s: status %d", e.Op, e.Status)
	if e.Code != "" {
		s += " " + e.Code
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// S3Store is a BlobStore over any S3-compatible endpoint (Garage, MinIO,
// SeaweedFS, Ceph RGW, AWS) using path-style addressing and SigV4.
//
// Put buffers one part at a time: a blob under PartSize is a single PutObject,
// anything larger is a multipart upload that is aborted on failure. Open does
// a HEAD and serves ReadAt with Range requests pinned to the object's ETag.
// There are no retries: the cache treats a failed store call as a cache miss.
type S3Store struct {
	// Endpoint is the base URL, e.g. https://s3.r1.tachyon.internal:3900.
	Endpoint string
	Bucket   string
	Region   string
	// Prefix is prepended to every object key (optional, e.g. "cache/").
	Prefix string
	Creds  S3Credentials
	// HTTP is the client to use; nil means http.DefaultClient (calls are
	// bounded by their context).
	HTTP *http.Client
	// PartSize is the multipart part size; 0 means DefaultS3PartSize.
	PartSize int64
	// Now is for tests.
	Now func() time.Time
}

func (s *S3Store) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return http.DefaultClient
}

func (s *S3Store) partSize() int64 {
	switch {
	case s.PartSize == 0:
		return DefaultS3PartSize
	case s.PartSize < minS3PartSize:
		return minS3PartSize
	}
	return s.PartSize
}

func (s *S3Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Validate checks the configuration without contacting the store.
func (s *S3Store) Validate() error {
	u, err := url.Parse(s.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("s3: endpoint must be an http(s) URL")
	}
	switch {
	case s.Bucket == "":
		return errors.New("s3: bucket is required")
	case s.Region == "":
		return errors.New("s3: region is required")
	case s.Creds.AccessKey == "" || s.Creds.SecretKey == "":
		return errors.New("s3: access key and secret key are required")
	}
	return nil
}

func (s *S3Store) objectURL(id string, query url.Values) (*url.URL, error) {
	if !validBlobID(id) {
		return nil, fmt.Errorf("invalid blob id %q", id)
	}
	u, err := url.Parse(s.Endpoint)
	if err != nil {
		return nil, err
	}
	base := strings.TrimRight(u.Path, "/")
	// validBlobID guarantees the key needs no escaping.
	u.Path = base + "/" + s.Bucket + "/" + s.Prefix + id
	u.RawPath = ""
	u.RawQuery = canonicalQuery(query)
	return u, nil
}

// do signs and sends a request. body may be nil. The caller closes the
// response body.
func (s *S3Store) do(ctx context.Context, method string, u *url.URL, body []byte, hdr http.Header) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rdr)
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	sum := sha256.Sum256(body)
	signV4(req, hex.EncodeToString(sum[:]), s.Creds, s.Region, s.now())
	// A url.Error from Do includes the URL, which holds no credentials.
	return s.client().Do(req)
}

// errFromResponse turns a non-success response into an *S3Error and closes it.
func errFromResponse(op string, resp *http.Response) error {
	defer resp.Body.Close()
	e := &S3Error{Op: op, Status: resp.StatusCode}
	if resp.Request != nil && resp.Request.Method != http.MethodHead {
		var x struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if xml.Unmarshal(b, &x) == nil {
			e.Code, e.Message = x.Code, x.Message
		}
	}
	return e
}

func ok2xx(resp *http.Response) bool { return resp.StatusCode/100 == 2 }

// Put streams r into the store and returns the number of bytes stored.
func (s *S3Store) Put(ctx context.Context, id string, r io.Reader) (int64, error) {
	if _, err := s.objectURL(id, nil); err != nil {
		return 0, err
	}
	ps := s.partSize()
	first := make([]byte, ps)
	n, err := io.ReadFull(r, first)
	switch {
	case err == io.EOF || err == io.ErrUnexpectedEOF:
		return int64(n), s.putObject(ctx, id, first[:n])
	case err != nil:
		return 0, err
	}
	// Exactly one full part read: multipart unless the stream ends here.
	var one [1]byte
	m, err := io.ReadFull(r, one[:])
	if err == io.EOF {
		return int64(n), s.putObject(ctx, id, first)
	}
	if err != nil {
		return 0, err
	}
	return s.putMultipart(ctx, id, io.MultiReader(bytes.NewReader(first), bytes.NewReader(one[:m]), r), ps)
}

func (s *S3Store) putObject(ctx context.Context, id string, b []byte) error {
	u, _ := s.objectURL(id, nil)
	resp, err := s.do(ctx, http.MethodPut, u, b, nil)
	if err != nil {
		return err
	}
	if !ok2xx(resp) {
		return errFromResponse("PutObject", resp)
	}
	resp.Body.Close()
	return nil
}

func (s *S3Store) putMultipart(ctx context.Context, id string, r io.Reader, ps int64) (total int64, err error) {
	u, _ := s.objectURL(id, url.Values{"uploads": {""}})
	resp, err := s.do(ctx, http.MethodPost, u, []byte{}, nil)
	if err != nil {
		return 0, err
	}
	if !ok2xx(resp) {
		return 0, errFromResponse("CreateMultipartUpload", resp)
	}
	var init struct {
		UploadID string `xml:"UploadId"`
	}
	derr := xml.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&init)
	resp.Body.Close()
	if derr != nil || init.UploadID == "" {
		return 0, errors.New("s3 CreateMultipartUpload: no upload id in response")
	}
	defer func() {
		if err != nil {
			// The request context may be the reason we failed.
			actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			au, _ := s.objectURL(id, url.Values{"uploadId": {init.UploadID}})
			if resp, aerr := s.do(actx, http.MethodDelete, au, nil, nil); aerr == nil {
				resp.Body.Close()
			}
		}
	}()

	type part struct {
		XMLName xml.Name `xml:"Part"`
		Number  int      `xml:"PartNumber"`
		ETag    string   `xml:"ETag"`
	}
	var parts []part
	buf := make([]byte, ps)
	for num := 1; ; num++ {
		if num > maxS3Parts {
			return 0, fmt.Errorf("blob exceeds %d parts of %d bytes", maxS3Parts, ps)
		}
		n, rerr := io.ReadFull(r, buf)
		if rerr == io.EOF {
			break
		}
		if rerr != nil && rerr != io.ErrUnexpectedEOF {
			return 0, rerr
		}
		pu, _ := s.objectURL(id, url.Values{"partNumber": {strconv.Itoa(num)}, "uploadId": {init.UploadID}})
		resp, err := s.do(ctx, http.MethodPut, pu, buf[:n], nil)
		if err != nil {
			return 0, err
		}
		if !ok2xx(resp) {
			return 0, errFromResponse("UploadPart", resp)
		}
		resp.Body.Close()
		et := resp.Header.Get("ETag")
		if et == "" {
			return 0, errors.New("s3 UploadPart: no ETag in response")
		}
		parts = append(parts, part{Number: num, ETag: et})
		total += int64(n)
		if rerr == io.ErrUnexpectedEOF {
			break
		}
	}
	body, err := xml.Marshal(struct {
		XMLName xml.Name `xml:"CompleteMultipartUpload"`
		Parts   []part
	}{Parts: parts})
	if err != nil {
		return 0, err
	}
	cu, _ := s.objectURL(id, url.Values{"uploadId": {init.UploadID}})
	resp, err = s.do(ctx, http.MethodPost, cu, body, nil)
	if err != nil {
		return 0, err
	}
	if !ok2xx(resp) {
		return 0, errFromResponse("CompleteMultipartUpload", resp)
	}
	// S3 may answer 200 with an error document in the body.
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if bytes.Contains(b, []byte("<Error>")) {
		var x struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		}
		_ = xml.Unmarshal(b, &x)
		return 0, &S3Error{Op: "CompleteMultipartUpload", Status: resp.StatusCode, Code: x.Code, Message: x.Message}
	}
	return total, nil
}

type s3Blob struct {
	s    *S3Store
	ctx  context.Context
	id   string
	size int64
	etag string
}

func (b *s3Blob) Size() int64  { return b.size }
func (b *s3Blob) Close() error { return nil }

// ReadAt implements io.ReaderAt with one ranged GET.
func (b *s3Blob) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("s3 blob: negative offset")
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= b.size {
		return 0, io.EOF
	}
	want := int64(len(p))
	short := false
	if off+want > b.size {
		want = b.size - off
		short = true
	}
	u, _ := b.s.objectURL(b.id, nil)
	hdr := http.Header{"Range": {fmt.Sprintf("bytes=%d-%d", off, off+want-1)}}
	if b.etag != "" {
		hdr.Set("If-Match", b.etag)
	}
	resp, err := b.s.do(b.ctx, http.MethodGet, u, nil, hdr)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return 0, ErrBlobNotFound
	case resp.StatusCode != http.StatusPartialContent:
		// A 200 means the store ignored Range; refuse rather than read
		// the whole object into the wrong offset.
		return 0, &S3Error{Op: "GetObject", Status: resp.StatusCode}
	}
	n, err := io.ReadFull(resp.Body, p[:want])
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return n, err
	}
	if short {
		return n, io.EOF
	}
	return n, nil
}

// head returns the size and ETag, or ErrBlobNotFound.
func (s *S3Store) head(ctx context.Context, id string) (int64, string, error) {
	u, err := s.objectURL(id, nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := s.do(ctx, http.MethodHead, u, nil, nil)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return 0, "", ErrBlobNotFound
	}
	if !ok2xx(resp) {
		return 0, "", &S3Error{Op: "HeadObject", Status: resp.StatusCode}
	}
	size, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	if err != nil || size < 0 {
		return 0, "", errors.New("s3 HeadObject: missing Content-Length")
	}
	return size, resp.Header.Get("ETag"), nil
}

// Open returns the blob, or ErrBlobNotFound. The context bounds every later
// ReadAt on the returned Blob.
func (s *S3Store) Open(ctx context.Context, id string) (Blob, error) {
	size, etag, err := s.head(ctx, id)
	if err != nil {
		return nil, err
	}
	return &s3Blob{s: s, ctx: ctx, id: id, size: size, etag: etag}, nil
}

// Delete removes the object. S3 deletes are idempotent, so it checks with a
// HEAD first to return ErrBlobNotFound for unknown ids.
func (s *S3Store) Delete(ctx context.Context, id string) error {
	if _, _, err := s.head(ctx, id); err != nil {
		return err
	}
	u, _ := s.objectURL(id, nil)
	resp, err := s.do(ctx, http.MethodDelete, u, nil, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return ErrBlobNotFound
	}
	if !ok2xx(resp) {
		return errFromResponse("DeleteObject", resp)
	}
	resp.Body.Close()
	return nil
}

// Check verifies the bucket is reachable with these credentials (HEAD bucket).
func (s *S3Store) Check(ctx context.Context) error {
	u, err := url.Parse(s.Endpoint)
	if err != nil {
		return err
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + s.Bucket + "/"
	resp, err := s.do(ctx, http.MethodHead, u, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if !ok2xx(resp) {
		return &S3Error{Op: "HeadBucket", Status: resp.StatusCode}
	}
	return nil
}

// --- AWS Signature Version 4 ---

func canonicalQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, awsEscape(k)+"="+awsEscape(v))
		}
	}
	return strings.Join(parts, "&")
}

// awsEscape percent-encodes everything except unreserved characters.
func awsEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func canonicalPath(p string) string {
	if p == "" {
		return "/"
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = awsEscape(s)
	}
	return strings.Join(segs, "/")
}

// signedHeaderNames are the request headers that take part in the signature,
// besides host and every x-amz-* header.
var signedHeaderNames = map[string]bool{"range": true, "if-match": true, "content-type": true}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// signV4 adds x-amz-date, x-amz-content-sha256 and Authorization to req.
func signV4(req *http.Request, payloadHash string, c S3Credentials, region string, now time.Time) {
	now = now.UTC()
	date := now.Format("20060102")
	stamp := now.Format("20060102T150405Z")
	req.Header.Set("X-Amz-Date", stamp)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	hdrs := map[string]string{"host": req.URL.Host}
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if signedHeaderNames[lk] || strings.HasPrefix(lk, "x-amz-") {
			hdrs[lk] = strings.TrimSpace(strings.Join(v, ","))
		}
	}
	names := make([]string, 0, len(hdrs))
	for k := range hdrs {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHdrs strings.Builder
	for _, k := range names {
		canonHdrs.WriteString(k + ":" + hdrs[k] + "\n")
	}
	signed := strings.Join(names, ";")

	canonical := strings.Join([]string{
		req.Method,
		canonicalPath(req.URL.Path),
		canonicalQueryFromRaw(req.URL.RawQuery),
		canonHdrs.String(),
		signed,
		payloadHash,
	}, "\n")
	scope := date + "/" + region + "/s3/aws4_request"
	ch := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + hex.EncodeToString(ch[:])
	k := hmacSHA256([]byte("AWS4"+c.SecretKey), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.AccessKey+"/"+scope+
		", SignedHeaders="+signed+", Signature="+sig)
}

func canonicalQueryFromRaw(raw string) string {
	q, _ := url.ParseQuery(raw)
	return canonicalQuery(q)
}
