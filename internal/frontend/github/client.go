package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// Client is a minimal GitHub App REST client implementing API.
type Client struct {
	AppID      int64
	PrivateKey *rsa.PrivateKey
	BaseURL    string // default https://api.github.com
	HTTP       *http.Client

	mu     sync.Mutex
	tokens map[int64]installationToken
}

type installationToken struct {
	token   string
	expires time.Time
}

var _ API = (*Client)(nil)

// ParsePrivateKey reads the App's PEM private key (PKCS#1 or PKCS#8).
func ParsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block in private key")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not RSA")
	}
	return rk, nil
}

func (c *Client) base() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return "https://api.github.com"
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// appJWT signs a short-lived RS256 JWT identifying the App.
func (c *Client) appJWT(now time.Time) (string, error) {
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(), // allow clock drift
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": strconv.FormatInt(c.AppID, 10),
	})
	signing := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.PrivateKey, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

func (c *Client) installationToken(ctx context.Context, inst int64) (string, error) {
	c.mu.Lock()
	if t, ok := c.tokens[inst]; ok && time.Until(t.expires) > 5*time.Minute {
		c.mu.Unlock()
		return t.token, nil
	}
	c.mu.Unlock()
	jwt, err := c.appJWT(time.Now())
	if err != nil {
		return "", err
	}
	var out struct {
		Token       string            `json:"token"`
		ExpiresAt   time.Time         `json:"expires_at"`
		Permissions map[string]string `json:"permissions"`
	}
	// Scope the token to exactly the App's least-privilege set, so it never
	// carries more even if the App registration drifts.
	body := map[string]any{"permissions": AppPermissions}
	if err := c.do(ctx, "Bearer "+jwt, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", inst), body, &out); err != nil {
		return "", fmt.Errorf("installation token: %w", err)
	}
	if err := CheckGranted(out.Permissions); err != nil {
		return "", fmt.Errorf("installation %d: %w", inst, err)
	}
	c.mu.Lock()
	if c.tokens == nil {
		c.tokens = map[int64]installationToken{}
	}
	c.tokens[inst] = installationToken{out.Token, out.ExpiresAt}
	c.mu.Unlock()
	return out.Token, nil
}

// APIError is a non-2xx response.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string { return fmt.Sprintf("github: HTTP %d: %s", e.Status, e.Body) }

func (c *Client) do(ctx context.Context, auth, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return &APIError{Status: resp.StatusCode, Body: string(data)}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (c *Client) asInstallation(ctx context.Context, inst int64, method, path string, body, out any) error {
	tok, err := c.installationToken(ctx, inst)
	if err != nil {
		return err
	}
	return c.do(ctx, "token "+tok, method, path, body, out)
}

func (c *Client) RunContext(ctx context.Context, inst int64, repo string, runID int64) (RunContext, error) {
	var run struct {
		Event          string `json:"event"`
		HeadBranch     string `json:"head_branch"`
		HeadRepository struct {
			FullName string `json:"full_name"`
		} `json:"head_repository"`
		Repository struct {
			FullName      string `json:"full_name"`
			DefaultBranch string `json:"default_branch"`
		} `json:"repository"`
		// Empty for pull requests from forks.
		PullRequests []struct {
			Number int `json:"number"`
			Base   struct {
				Ref string `json:"ref"`
			} `json:"base"`
		} `json:"pull_requests"`
	}
	if err := c.asInstallation(ctx, inst, http.MethodGet, fmt.Sprintf("/repos/%s/actions/runs/%d", repo, runID), nil, &run); err != nil {
		return RunContext{}, err
	}
	rc := RunContext{
		Event:         run.Event,
		HeadBranch:    run.HeadBranch,
		HeadRepo:      run.HeadRepository.FullName,
		BaseRepo:      run.Repository.FullName,
		DefaultBranch: run.Repository.DefaultBranch,
	}
	if len(run.PullRequests) > 0 {
		rc.PRNumber, rc.BaseBranch = run.PullRequests[0].Number, run.PullRequests[0].Base.Ref
	}
	if rc.HeadBranch != "" && rc.HeadRepo == rc.BaseRepo {
		var br struct {
			Protected bool `json:"protected"`
		}
		err := c.asInstallation(ctx, inst, http.MethodGet, fmt.Sprintf("/repos/%s/branches/%s", repo, url.PathEscape(rc.HeadBranch)), nil, &br)
		var apiErr *APIError
		if err != nil && !(errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound) {
			return RunContext{}, err
		}
		rc.Protected = br.Protected
	}
	return rc, nil
}

func (c *Client) CreateJITConfig(ctx context.Context, inst int64, repo, name string, labels []string) (JITRunner, error) {
	var out struct {
		Runner struct {
			ID int64 `json:"id"`
		} `json:"runner"`
		EncodedJITConfig string `json:"encoded_jit_config"`
	}
	body := map[string]any{
		"name":            name,
		"runner_group_id": 1, // repository runners use the default group
		"labels":          labels,
		"work_folder":     "_work",
	}
	if err := c.asInstallation(ctx, inst, http.MethodPost, fmt.Sprintf("/repos/%s/actions/runners/generate-jitconfig", repo), body, &out); err != nil {
		return JITRunner{}, err
	}
	return JITRunner{RunnerID: out.Runner.ID, JITConfig: []byte(out.EncodedJITConfig)}, nil
}

func (c *Client) ListRunners(ctx context.Context, inst int64, repo string) ([]Runner, error) {
	var all []Runner
	for page := 1; ; page++ {
		var out struct {
			Runners []struct {
				ID   int64  `json:"id"`
				Name string `json:"name"`
				Busy bool   `json:"busy"`
			} `json:"runners"`
		}
		if err := c.asInstallation(ctx, inst, http.MethodGet, fmt.Sprintf("/repos/%s/actions/runners?per_page=100&page=%d", repo, page), nil, &out); err != nil {
			return nil, err
		}
		for _, r := range out.Runners {
			all = append(all, Runner{ID: r.ID, Name: r.Name, Busy: r.Busy})
		}
		if len(out.Runners) < 100 {
			return all, nil
		}
	}
}

func (c *Client) DeleteRunner(ctx context.Context, inst int64, repo string, runnerID int64) error {
	err := c.asInstallation(ctx, inst, http.MethodDelete, fmt.Sprintf("/repos/%s/actions/runners/%d", repo, runnerID), nil, nil)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		return nil // already gone
	}
	return err
}
