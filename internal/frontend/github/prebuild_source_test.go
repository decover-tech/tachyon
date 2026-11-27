package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

const wideTree = 40000

func TestTenantInstallation(t *testing.T) {
	if id, err := tenantInstallation("gh_inst_42"); err != nil || id != 42 {
		t.Errorf("got %d, %v", id, err)
	}
	for _, bad := range []string{"", "gh_inst_", "gh_inst_x", "gl_42", "gh_inst_42x", "gh_inst_-1"} {
		if _, err := tenantInstallation(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

func TestPrebuildSource(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	big := strings.Repeat("x", 100)
	mux := http.NewServeMux()
	var scopedBodies []map[string]any
	mux.HandleFunc("POST /app/installations/42/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["repositories"] != nil {
			scopedBodies = append(scopedBodies, body)
			perms := map[string]string{"contents": "read", "metadata": "read"}
			if body["repositories"].([]any)[0] == "wide" {
				perms["actions"] = "write"
			}
			json.NewEncoder(w).Encode(map[string]any{"token": "ghs_scoped", "expires_at": time.Now().Add(time.Hour), "permissions": perms})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"token": "inst-tok", "expires_at": time.Now().Add(time.Hour)})
	})
	mux.HandleFunc("GET /repos/acme/api", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"default_branch":"trunk"}`))
	})
	mux.HandleFunc("GET /repos/acme/api/commits/trunk", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"sha":"` + sha + `"}`))
	})
	mux.HandleFunc("GET /repos/acme/api/branches/{b}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("b") {
		case "release/1":
			w.Write([]byte(`{"protected":true}`))
		case "gone":
			http.Error(w, `{"message":"Branch not found"}`, http.StatusNotFound)
		default:
			w.Write([]byte(`{"protected":false}`))
		}
	})
	mux.HandleFunc("GET /repos/acme/api/git/trees/{sha}", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("recursive") != "1" {
			t.Error("tree not recursive")
		}
		if r.PathValue("sha") == "huge" {
			w.Write([]byte(`{"tree":[],"truncated":true}`))
			return
		}
		if r.PathValue("sha") == "wide" {
			// A real repo's tree is several MB of JSON.
			fmt.Fprint(w, `{"tree":[`)
			for i := range wideTree {
				if i > 0 {
					fmt.Fprint(w, ",")
				}
				fmt.Fprintf(w, `{"path":"src/components/some/deeply/nested/module_%05d.tsx","type":"blob"}`, i)
			}
			fmt.Fprint(w, `],"truncated":false}`)
			return
		}
		w.Write([]byte(`{"tree":[{"path":"go.sum","type":"blob"},{"path":"web","type":"tree"},{"path":"web/package-lock.json","type":"blob"}],"truncated":false}`))
	})
	mux.HandleFunc("GET /repos/acme/api/contents/{path...}", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ref") != sha {
			t.Errorf("ref = %q", r.URL.Query().Get("ref"))
		}
		switch r.PathValue("path") {
		case ".tachyon/prebuild.yml":
			// GitHub wraps base64 at 60 columns.
			enc := base64.StdEncoding.EncodeToString([]byte("version: 1\n"))
			json.NewEncoder(w).Encode(map[string]string{"type": "file", "encoding": "base64", "content": enc[:4] + "\n" + enc[4:] + "\n", "sha": "s1"})
		case "big.lock":
			json.NewEncoder(w).Encode(map[string]string{"type": "file", "encoding": "none", "content": "", "sha": "blobsha"})
		case "dir":
			w.Write([]byte(`[{"type":"file"}]`))
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	})
	mux.HandleFunc("GET /repos/acme/api/git/blobs/blobsha", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(big))})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	s := &PrebuildSource{Client: &Client{AppID: 1, PrivateKey: testKey(t), BaseURL: srv.URL}}
	ctx := context.Background()
	const tenant = "gh_inst_42"

	if head, err := s.DefaultHead(ctx, tenant, "acme/api"); err != nil || head != sha {
		t.Errorf("DefaultHead = %q, %v", head, err)
	}
	for b, want := range map[string]bool{"release/1": true, "dev": false, "gone": false} {
		if got, err := s.Protected(ctx, tenant, "acme/api", b); err != nil || got != want {
			t.Errorf("Protected(%s) = %v, %v", b, got, err)
		}
	}
	files, err := s.Files(ctx, tenant, "acme/api", sha)
	if err != nil || !reflect.DeepEqual(files, []string{"go.sum", "web/package-lock.json"}) {
		t.Errorf("Files = %v, %v", files, err)
	}
	if files, err := s.Files(ctx, tenant, "acme/api", "wide"); err != nil || len(files) != wideTree {
		t.Errorf("Files(wide) = %d files, %v", len(files), err)
	}
	if _, err := s.Files(ctx, tenant, "acme/api", "huge"); err == nil {
		t.Error("truncated tree should fail")
	}
	if b, err := s.ReadFile(ctx, tenant, "acme/api", sha, ".tachyon/prebuild.yml"); err != nil || string(b) != "version: 1\n" {
		t.Errorf("ReadFile = %q, %v", b, err)
	}
	if b, err := s.ReadFile(ctx, tenant, "acme/api", sha, "big.lock"); err != nil || string(b) != big {
		t.Errorf("ReadFile big = %d bytes, %v", len(b), err)
	}
	if _, err := s.ReadFile(ctx, tenant, "acme/api", sha, "missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing file err = %v", err)
	}
	if _, err := s.ReadFile(ctx, tenant, "acme/api", sha, "dir"); err == nil {
		t.Error("directory should not read as a file")
	}

	tok, err := s.CloneToken(ctx, tenant, "acme/api")
	if err != nil {
		t.Fatal(err)
	}
	if string(tok.Token) != "ghs_scoped" || tok.URL != "https://github.com/acme/api.git" || tok.ExpiresAt.IsZero() {
		t.Errorf("CloneToken = %+v", tok)
	}
	if len(scopedBodies) != 1 || !reflect.DeepEqual(scopedBodies[0]["permissions"], map[string]any{"contents": "read"}) ||
		!reflect.DeepEqual(scopedBodies[0]["repositories"], []any{"api"}) {
		t.Errorf("token request = %v", scopedBodies)
	}
	if _, err := s.CloneToken(ctx, tenant, "acme/wide"); err == nil {
		t.Error("token with extra permissions should be refused")
	}
	if _, err := s.CloneToken(ctx, "gl_1", "acme/api"); err == nil {
		t.Error("non-GitHub tenant should fail")
	}
}
