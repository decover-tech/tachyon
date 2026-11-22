package cache

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/decover-tech/tachyon/internal/jobapi"
)

// Signer mints and checks the two kinds of credential the cache service
// hands out, both HMAC-SHA256 under one regional key:
//
//   - job tokens, given to each VM, which carry the job's cache Scope; and
//   - signed blob URLs, returned by the Twirp API, which let the toolkit
//     upload or download exactly one blob for a short time.
//
// The control plane (which mints job tokens) and the regional cache service
// (which checks them) share the key.
type Signer struct {
	Key []byte
	Now func() time.Time
}

const tokenPrefix = "tcv1."

var (
	ErrBadToken     = errors.New("invalid cache credential")
	ErrExpiredToken = errors.New("expired cache credential")
)

// Claims are what a job token says about its holder.
type Claims struct {
	JobID         string       `json:"job"`
	Repo          string       `json:"repo"`
	Ref           string       `json:"ref"`
	BaseRef       string       `json:"base,omitempty"`
	DefaultBranch string       `json:"def"`
	Trust         jobapi.Trust `json:"trust"`
	Expires       int64        `json:"exp"` // unix seconds
}

// Scope is the cache scope the token grants.
func (c Claims) Scope() Scope {
	return Scope{Repo: c.Repo, Ref: c.Ref, BaseRef: c.BaseRef, DefaultBranch: c.DefaultBranch, Trust: c.Trust}
}

func (s *Signer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// mac authenticates parts with a domain label, NUL-separated so fields
// can't be shifted between each other.
func (s *Signer) mac(parts ...string) []byte {
	m := hmac.New(sha256.New, s.Key)
	for _, p := range parts {
		m.Write([]byte(p))
		m.Write([]byte{0})
	}
	return m.Sum(nil)
}

// MintJobToken returns a token for one job's scope, valid for ttl.
func (s *Signer) MintJobToken(jobID string, scope Scope, ttl time.Duration) (string, error) {
	if len(s.Key) == 0 {
		return "", errors.New("cache signer has no key")
	}
	if scope.Repo == "" {
		return "", errors.New("cache token needs a repo")
	}
	switch scope.Trust {
	case jobapi.Trusted, jobapi.Untrusted, jobapi.UntrustedFork:
	default:
		return "", errors.New("cache token needs a trust level")
	}
	c := Claims{
		JobID: jobID, Repo: scope.Repo, Ref: scope.Ref, BaseRef: scope.BaseRef,
		DefaultBranch: scope.DefaultBranch, Trust: scope.Trust,
		Expires: s.now().Add(ttl).Unix(),
	}
	body, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(body)
	sig := base64.RawURLEncoding.EncodeToString(s.mac("job", payload))
	return tokenPrefix + payload + "." + sig, nil
}

// VerifyJobToken checks a token's signature and expiry and returns its claims.
func (s *Signer) VerifyJobToken(tok string) (Claims, error) {
	rest, ok := strings.CutPrefix(tok, tokenPrefix)
	if !ok || len(s.Key) == 0 {
		return Claims{}, ErrBadToken
	}
	payload, sigStr, ok := strings.Cut(rest, ".")
	if !ok {
		return Claims{}, ErrBadToken
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigStr)
	if err != nil || !hmac.Equal(sig, s.mac("job", payload)) {
		return Claims{}, ErrBadToken
	}
	body, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return Claims{}, ErrBadToken
	}
	var c Claims
	if err := json.Unmarshal(body, &c); err != nil || c.Repo == "" {
		return Claims{}, ErrBadToken
	}
	if s.now().Unix() >= c.Expires {
		return Claims{}, ErrExpiredToken
	}
	return c, nil
}

// Blob URL permissions.
const (
	permRead  = "r"
	permWrite = "w"
)

// blobQuery returns the signed query string for one blob and permission.
func (s *Signer) blobQuery(blobID, perm string, ttl time.Duration) string {
	exp := strconv.FormatInt(s.now().Add(ttl).Unix(), 10)
	sig := base64.RawURLEncoding.EncodeToString(s.mac("blob", perm, blobID, exp))
	return "sp=" + perm + "&se=" + exp + "&sig=" + sig
}

// verifyBlob checks a signed blob URL's query values.
func (s *Signer) verifyBlob(blobID, perm, exp, sigStr string) error {
	if len(s.Key) == 0 {
		return ErrBadToken
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigStr)
	if err != nil || !hmac.Equal(sig, s.mac("blob", perm, blobID, exp)) {
		return ErrBadToken
	}
	e, err := strconv.ParseInt(exp, 10, 64)
	if err != nil {
		return ErrBadToken
	}
	if s.now().Unix() >= e {
		return ErrExpiredToken
	}
	return nil
}
