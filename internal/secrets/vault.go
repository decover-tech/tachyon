package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Vault stores secrets in a HashiCorp Vault or OpenBao KV version 2 secrets
// engine, over its HTTP API.
//
// Every Put writes a new key <Prefix>/<path>/<id> with max_versions=1 and
// delete_version_after set to the path's TTL, so the key store itself
// deletes the value when the TTL passes, even if the control plane dies.
// Get also checks an expires_at field so an expired value is never returned
// before the server's sweep runs. Delete destroys the key and all its
// metadata.
//
// The token needs create, update, read and delete on <Mount>/data/<Prefix>/* and
// <Mount>/metadata/<Prefix>/*, and nothing else.
type Vault struct {
	Addr      string // e.g. https://vault.internal:8200
	Mount     string // KV v2 mount; empty means "secret"
	Prefix    string // key prefix under the mount; empty means "tachyon"
	Namespace string // Vault Enterprise / OpenBao namespace; optional

	// Token authenticates requests. If TokenFile is set it is read on every
	// request instead, so a Vault Agent sidecar can rotate the token.
	// Token is Redacted so printing a Vault never shows it.
	Token     Redacted
	TokenFile string

	Policy Policy           // zero means DefaultPolicy
	HTTP   *http.Client     // nil means a client with a 10s timeout
	Now    func() time.Time // for tests; nil means time.Now
}

var _ Store = (*Vault)(nil)

var defaultVaultHTTP = &http.Client{Timeout: 10 * time.Second}

// maxVaultResponse bounds how much of a response body is read.
const maxVaultResponse = 1 << 20

func (v *Vault) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

func (v *Vault) key(path, id string) string {
	prefix := strings.Trim(v.Prefix, "/")
	if prefix == "" {
		prefix = "tachyon"
	}
	return prefix + "/" + path + "/" + id
}

func (v *Vault) url(kind, key string) string {
	mount := strings.Trim(v.Mount, "/")
	if mount == "" {
		mount = "secret"
	}
	return strings.TrimRight(v.Addr, "/") + "/v1/" + mount + "/" + kind + "/" + key
}

func (v *Vault) token() (string, error) {
	if v.TokenFile != "" {
		b, err := os.ReadFile(v.TokenFile)
		if err != nil {
			return "", fmt.Errorf("read vault token file: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	if len(v.Token) == 0 {
		return "", errors.New("no vault token configured")
	}
	return string(v.Token), nil
}

type kvValue struct {
	Value     string    `json:"value"` // base64
	ExpiresAt time.Time `json:"expires_at"`
}

func (v *Vault) Put(ctx context.Context, path string, value []byte) (string, error) {
	ref, err := NewRef(path)
	if err != nil {
		return "", err
	}
	_, id, err := ParseRef(ref)
	if err != nil {
		return "", err
	}
	key := v.key(path, id)
	ttl := v.Policy.TTL(path)

	// Metadata first, so the server-side TTL is in place before the value
	// exists.
	meta := map[string]any{
		"max_versions":         1,
		"delete_version_after": fmt.Sprintf("%ds", int64(ttl/time.Second)),
	}
	if _, err := v.do(ctx, http.MethodPost, v.url("metadata", key), meta); err != nil {
		return "", fmt.Errorf("store %s: %w", ref, err)
	}
	body := map[string]any{
		"options": map[string]any{"cas": 0}, // only if the key is new
		"data": kvValue{
			Value:     base64.StdEncoding.EncodeToString(value),
			ExpiresAt: v.now().Add(ttl).UTC(),
		},
	}
	if _, err := v.do(ctx, http.MethodPost, v.url("data", key), body); err != nil {
		_ = v.Delete(ctx, ref)
		return "", fmt.Errorf("store %s: %w", ref, err)
	}
	return ref, nil
}

func (v *Vault) Get(ctx context.Context, ref string) ([]byte, error) {
	path, id, err := ParseRef(ref)
	if err != nil {
		return nil, err
	}
	raw, err := v.do(ctx, http.MethodGet, v.url("data", v.key(path, id)), nil)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", ref, err)
	}
	var resp struct {
		Data struct {
			Data     *kvValue `json:"data"`
			Metadata struct {
				DeletionTime string `json:"deletion_time"`
				Destroyed    bool   `json:"destroyed"`
			} `json:"metadata"`
		} `json:"data"`
	}
	// Never wrap the decode error: it can quote the body.
	if json.Unmarshal(raw, &resp) != nil {
		return nil, fmt.Errorf("resolve %s: malformed key store response", ref)
	}
	d := resp.Data
	if d.Data == nil || d.Metadata.Destroyed || d.Metadata.DeletionTime != "" {
		return nil, fmt.Errorf("%s: %w", ref, ErrNotFound)
	}
	if d.Data.ExpiresAt.IsZero() || !v.now().Before(d.Data.ExpiresAt) {
		_ = v.Delete(ctx, ref)
		return nil, fmt.Errorf("%s: %w", ref, ErrExpired)
	}
	val, err := base64.StdEncoding.DecodeString(d.Data.Value)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: malformed key store value", ref)
	}
	return val, nil
}

func (v *Vault) Delete(ctx context.Context, ref string) error {
	path, id, err := ParseRef(ref)
	if err != nil {
		return err
	}
	_, err = v.do(ctx, http.MethodDelete, v.url("metadata", v.key(path, id)), nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete %s: %w", ref, err)
	}
	return nil
}

// do sends one request. Errors carry the method and status only: never the
// request body (it holds the secret) or the response body (it may).
func (v *Vault) do(ctx context.Context, method, url string, body any) ([]byte, error) {
	tok, err := v.token()
	if err != nil {
		return nil, err
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, errors.New("encode key store request")
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Vault-Token", tok)
	req.Header.Set("X-Vault-Request", "true")
	if v.Namespace != "" {
		req.Header.Set("X-Vault-Namespace", v.Namespace)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := v.HTTP
	if hc == nil {
		hc = defaultVaultHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("key store %s: %w", method, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxVaultResponse))
	if err != nil {
		return nil, fmt.Errorf("key store %s: read response: %w", method, err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, ErrNotFound
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, fmt.Errorf("key store %s: status %d", method, resp.StatusCode)
	}
	return raw, nil
}
