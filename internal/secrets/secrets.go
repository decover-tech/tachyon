// Package secrets keeps secret material (GitHub JIT runner configs, tokens)
// out of job specs, logs and process state. Values live in a key store and
// travel only as opaque secret:// references.
//
// Two stores implement Store: Vault talks to a HashiCorp Vault or OpenBao
// KV v2 engine (the production key store, see docs/decisions/0002-key-store.md)
// and Mem keeps values in process memory for development and tests. Both
// expire every value after a TTL chosen by Policy, so a reference that leaks
// stops working quickly.
package secrets

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Scheme prefixes every reference.
const Scheme = "secret://"

var (
	// ErrNotFound means the reference names no live secret.
	ErrNotFound = errors.New("secret not found")
	// ErrExpired means the secret existed but its TTL has passed.
	ErrExpired = errors.New("secret expired")
	// ErrInvalidRef means the string is not a well-formed secret:// reference.
	ErrInvalidRef = errors.New("invalid secret reference")
)

// Store stores secret values and hands out references to them.
//
// Implementations must never include a secret value in an error, a log
// record or a String method. References are not secret on their own (reading
// one needs key-store credentials), so errors may name them.
type Store interface {
	// Put stores value under a logical path (for example "jit/<runner>")
	// and returns a fresh, unguessable reference. The value expires after
	// the store's TTL for that path.
	Put(ctx context.Context, path string, value []byte) (ref string, err error)
	// Get resolves a reference. It returns ErrNotFound or ErrExpired
	// (possibly wrapped) when the value is gone.
	Get(ctx context.Context, ref string) ([]byte, error)
	// Delete removes the value. Deleting a missing reference is not an
	// error.
	Delete(ctx context.Context, ref string) error
}

// Default TTLs. A JIT runner config is consumed within seconds of dispatch,
// and the GitHub adapter mints a new one if a job is not picked up within
// its reconcile window (2 minutes by default), so 10 minutes leaves margin
// without letting a stolen config live long.
const (
	DefaultJITTTL = 10 * time.Minute
	DefaultTTL    = time.Hour
)

// Policy maps a secret path to its TTL by longest matching prefix.
type Policy struct {
	// Default applies when no prefix matches. Zero means DefaultTTL.
	Default time.Duration
	// Prefixes maps a path prefix (for example "jit/") to its TTL.
	Prefixes map[string]time.Duration
}

// DefaultPolicy gives JIT runner configs a short TTL and everything else
// DefaultTTL.
func DefaultPolicy() Policy {
	return Policy{Default: DefaultTTL, Prefixes: map[string]time.Duration{"jit/": DefaultJITTTL}}
}

// TTL returns the TTL for path.
func (p Policy) TTL(path string) time.Duration {
	if p.Default == 0 && p.Prefixes == nil {
		p = DefaultPolicy()
	}
	best, ttl := -1, p.Default
	for prefix, d := range p.Prefixes {
		if strings.HasPrefix(path, prefix) && len(prefix) > best {
			best, ttl = len(prefix), d
		}
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return ttl
}

// NewRef returns a fresh reference for path: secret://<path>#<random id>.
func NewRef(path string) (string, error) {
	if err := ValidatePath(path); err != nil {
		return "", err
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return Scheme + path + "#" + hex.EncodeToString(b[:]), nil
}

// ParseRef splits a reference into its path and id.
func ParseRef(ref string) (path, id string, err error) {
	rest, ok := strings.CutPrefix(ref, Scheme)
	if !ok {
		return "", "", fmt.Errorf("%w: missing %s scheme", ErrInvalidRef, Scheme)
	}
	path, id, ok = strings.Cut(rest, "#")
	if !ok || id == "" {
		return "", "", fmt.Errorf("%w: missing id", ErrInvalidRef)
	}
	if err := ValidatePath(path); err != nil {
		return "", "", err
	}
	if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
		return "", "", fmt.Errorf("%w: malformed id", ErrInvalidRef)
	}
	return path, id, nil
}

// ValidatePath accepts slash-separated segments of letters, digits and
// "._-", with no empty, "." or ".." segment. That keeps paths safe to embed
// in a key-store URL.
func ValidatePath(path string) error {
	if path == "" {
		return fmt.Errorf("%w: empty path", ErrInvalidRef)
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("%w: bad path segment in %q", ErrInvalidRef, path)
		}
		for _, r := range seg {
			ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
				r == '.' || r == '_' || r == '-'
			if !ok {
				return fmt.Errorf("%w: bad character in %q", ErrInvalidRef, path)
			}
		}
	}
	return nil
}

// Redacted wraps secret bytes so that printing or logging them never shows
// the value. Use it for any struct field or log attribute that might hold a
// secret.
type Redacted []byte

const redactedText = "[REDACTED]"

func (Redacted) String() string                 { return redactedText }
func (Redacted) GoString() string               { return redactedText }
func (Redacted) LogValue() slog.Value           { return slog.StringValue(redactedText) }
func (Redacted) MarshalText() ([]byte, error)   { return []byte(redactedText), nil }
func (r Redacted) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(redactedText)) }
func (r Redacted) Reveal() []byte               { return []byte(r) }
func (r Redacted) MarshalJSON() ([]byte, error) { return []byte(`"` + redactedText + `"`), nil }
