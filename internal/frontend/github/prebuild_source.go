package github

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/decover-tech/tachyon/internal/prebuild"
)

// PrebuildSource reads repos for the prebuild manager through the GitHub
// App, using the contents-read permission.
type PrebuildSource struct {
	Client *Client
	// GitHost is the host clone URLs point at. Default github.com.
	GitHost string
}

var _ prebuild.Source = (*PrebuildSource)(nil)

// tenantInstallation parses the tenant the adapter assigns, gh_inst_<id>.
func tenantInstallation(tenant string) (int64, error) {
	var id int64
	if _, err := fmt.Sscanf(tenant, "gh_inst_%d", &id); err != nil || id <= 0 || fmt.Sprintf("gh_inst_%d", id) != tenant {
		return 0, fmt.Errorf("tenant %q is not a GitHub installation", tenant)
	}
	return id, nil
}

func isNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}

// escapePath escapes each segment of a repo-relative path.
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

func (s *PrebuildSource) DefaultHead(ctx context.Context, tenant, repo string) (string, error) {
	inst, err := tenantInstallation(tenant)
	if err != nil {
		return "", err
	}
	var r struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := s.Client.asInstallation(ctx, inst, http.MethodGet, "/repos/"+repo, nil, &r); err != nil {
		return "", err
	}
	if r.DefaultBranch == "" {
		return "", fmt.Errorf("%s has no default branch", repo)
	}
	var c struct {
		SHA string `json:"sha"`
	}
	if err := s.Client.asInstallation(ctx, inst, http.MethodGet, fmt.Sprintf("/repos/%s/commits/%s", repo, url.PathEscape(r.DefaultBranch)), nil, &c); err != nil {
		return "", err
	}
	return c.SHA, nil
}

func (s *PrebuildSource) Protected(ctx context.Context, tenant, repo, branch string) (bool, error) {
	inst, err := tenantInstallation(tenant)
	if err != nil {
		return false, err
	}
	var br struct {
		Protected bool `json:"protected"`
	}
	err = s.Client.asInstallation(ctx, inst, http.MethodGet, fmt.Sprintf("/repos/%s/branches/%s", repo, url.PathEscape(branch)), nil, &br)
	if isNotFound(err) {
		return false, nil
	}
	return br.Protected, err
}

func (s *PrebuildSource) Files(ctx context.Context, tenant, repo, commit string) ([]string, error) {
	inst, err := tenantInstallation(tenant)
	if err != nil {
		return nil, err
	}
	var t struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
		Truncated bool `json:"truncated"`
	}
	if err := s.Client.asInstallation(ctx, inst, http.MethodGet, fmt.Sprintf("/repos/%s/git/trees/%s?recursive=1", repo, url.PathEscape(commit)), nil, &t); err != nil {
		return nil, err
	}
	if t.Truncated {
		// A partial list could silently drop key files and give a wrong key.
		return nil, fmt.Errorf("%s@%s: tree too large to list", repo, commit)
	}
	var out []string
	for _, e := range t.Tree {
		if e.Type == "blob" {
			out = append(out, e.Path)
		}
	}
	return out, nil
}

func (s *PrebuildSource) ReadFile(ctx context.Context, tenant, repo, commit, path string) ([]byte, error) {
	inst, err := tenantInstallation(tenant)
	if err != nil {
		return nil, err
	}
	var f struct {
		Type     string `json:"type"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
		SHA      string `json:"sha"`
	}
	err = s.Client.asInstallation(ctx, inst, http.MethodGet, fmt.Sprintf("/repos/%s/contents/%s?ref=%s", repo, escapePath(path), url.QueryEscape(commit)), nil, &f)
	if isNotFound(err) {
		return nil, fmt.Errorf("%s@%s:%s: %w", repo, commit, path, fs.ErrNotExist)
	}
	if err != nil {
		return nil, err
	}
	if f.Type != "file" {
		return nil, fmt.Errorf("%s@%s:%s: not a file: %w", repo, commit, path, fs.ErrNotExist)
	}
	if f.Encoding != "base64" {
		// Files over 1 MB come back without content; fetch the blob.
		var b struct {
			Encoding string `json:"encoding"`
			Content  string `json:"content"`
		}
		if err := s.Client.asInstallation(ctx, inst, http.MethodGet, fmt.Sprintf("/repos/%s/git/blobs/%s", repo, url.PathEscape(f.SHA)), nil, &b); err != nil {
			return nil, err
		}
		if b.Encoding != "base64" {
			return nil, fmt.Errorf("%s@%s:%s: unexpected blob encoding %q", repo, commit, path, b.Encoding)
		}
		f.Content = b.Content
	}
	// GitHub wraps base64 content at 60 columns.
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(f.Content, "\n", ""))
}

// CloneToken mints an installation token restricted to this one repo and
// to contents:read. GitHub expires it after an hour; it is minted per build
// and never cached.
func (s *PrebuildSource) CloneToken(ctx context.Context, tenant, repo string) (prebuild.CloneToken, error) {
	inst, err := tenantInstallation(tenant)
	if err != nil {
		return prebuild.CloneToken{}, err
	}
	_, name, ok := strings.Cut(repo, "/")
	if !ok || name == "" {
		return prebuild.CloneToken{}, fmt.Errorf("repo %q: want owner/name", repo)
	}
	jwt, err := s.Client.appJWT(time.Now())
	if err != nil {
		return prebuild.CloneToken{}, err
	}
	body := map[string]any{
		"repositories": []string{name},
		"permissions":  map[string]string{"contents": "read"},
	}
	var out struct {
		Token       string            `json:"token"`
		ExpiresAt   time.Time         `json:"expires_at"`
		Permissions map[string]string `json:"permissions"`
	}
	if err := s.Client.do(ctx, "Bearer "+jwt, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", inst), body, &out); err != nil {
		return prebuild.CloneToken{}, fmt.Errorf("clone token: %w", err)
	}
	// Refuse a token wider than asked for.
	for perm, level := range out.Permissions {
		if perm != "contents" && perm != "metadata" || level != "read" {
			return prebuild.CloneToken{}, fmt.Errorf("clone token has %s:%s; want contents:read only", perm, level)
		}
	}
	if out.Token == "" {
		return prebuild.CloneToken{}, errors.New("clone token: empty token")
	}
	host := s.GitHost
	if host == "" {
		host = "github.com"
	}
	return prebuild.CloneToken{
		URL:       fmt.Sprintf("https://%s/%s.git", host, repo),
		Token:     []byte(out.Token),
		ExpiresAt: out.ExpiresAt,
	}, nil
}
