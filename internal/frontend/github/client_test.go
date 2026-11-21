package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestParsePrivateKey(t *testing.T) {
	k := testKey(t)
	pkcs1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	pkcs8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	for name, b := range map[string][]byte{"pkcs1": pkcs1, "pkcs8": pkcs8} {
		got, err := ParsePrivateKey(b)
		if err != nil || !got.Equal(k) {
			t.Errorf("%s: err=%v", name, err)
		}
	}
	if _, err := ParsePrivateKey([]byte("nope")); err == nil {
		t.Error("expected error for non-PEM")
	}
}

func TestAppJWT(t *testing.T) {
	k := testKey(t)
	c := &Client{AppID: 123, PrivateKey: k}
	now := time.Unix(1_800_000_000, 0)
	tok, err := c.appJWT(now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d parts", len(parts))
	}
	enc := base64.RawURLEncoding
	hdr, _ := enc.DecodeString(parts[0])
	if string(hdr) != `{"alg":"RS256","typ":"JWT"}` {
		t.Errorf("header = %s", hdr)
	}
	var claims struct {
		Iat, Exp int64
		Iss      string
	}
	raw, _ := enc.DecodeString(parts[1])
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Iss != "123" || claims.Iat != now.Unix()-60 || claims.Exp != now.Add(9*time.Minute).Unix() {
		t.Errorf("claims = %+v", claims)
	}
	sig, _ := enc.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&k.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Errorf("signature: %v", err)
	}
}

func TestClientEndpoints(t *testing.T) {
	var tokenCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/42/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("token request auth = %q", r.Header.Get("Authorization"))
		}
		tokenCalls.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"token": "inst-tok", "expires_at": time.Now().Add(time.Hour)})
	})
	checkAuth := func(r *http.Request) {
		if r.Header.Get("Authorization") != "token inst-tok" {
			t.Errorf("%s auth = %q", r.URL.Path, r.Header.Get("Authorization"))
		}
	}
	mux.HandleFunc("GET /repos/acme/api/actions/runs/7", func(w http.ResponseWriter, r *http.Request) {
		checkAuth(r)
		w.Write([]byte(`{"event":"push","head_branch":"release/1","head_repository":{"full_name":"acme/api"},
			"repository":{"full_name":"acme/api","default_branch":"main"}}`))
	})
	mux.HandleFunc("GET /repos/acme/api/branches/{b}", func(w http.ResponseWriter, r *http.Request) {
		checkAuth(r)
		if r.PathValue("b") != "release/1" {
			t.Errorf("branch = %q", r.PathValue("b"))
		}
		w.Write([]byte(`{"protected":true}`))
	})
	mux.HandleFunc("POST /repos/acme/api/actions/runners/generate-jitconfig", func(w http.ResponseWriter, r *http.Request) {
		checkAuth(r)
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["name"] != "tachyon-x" {
			t.Errorf("jit body = %v", body)
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"runner":{"id":55},"encoded_jit_config":"ENC"}`))
	})
	mux.HandleFunc("DELETE /repos/acme/api/actions/runners/{id}", func(w http.ResponseWriter, r *http.Request) {
		checkAuth(r)
		if r.PathValue("id") == "404" {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		if r.PathValue("id") == "500" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /repos/acme/api/actions/runners", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"runners":[{"id":1,"name":"tachyon-a","busy":true}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &Client{AppID: 1, PrivateKey: testKey(t), BaseURL: srv.URL}
	ctx := context.Background()

	rc, err := c.RunContext(ctx, 42, "acme/api", 7)
	if err != nil {
		t.Fatal(err)
	}
	want := RunContext{Event: "push", HeadBranch: "release/1", HeadRepo: "acme/api", BaseRepo: "acme/api", DefaultBranch: "main", Protected: true}
	if rc != want {
		t.Errorf("RunContext = %+v", rc)
	}

	jit, err := c.CreateJITConfig(ctx, 42, "acme/api", "tachyon-x", []string{"tachyon-ubuntu-24.04-2x"})
	if err != nil || jit.RunnerID != 55 || string(jit.JITConfig) != "ENC" {
		t.Errorf("CreateJITConfig = %+v, %v", jit, err)
	}

	runners, err := c.ListRunners(ctx, 42, "acme/api")
	if err != nil || len(runners) != 1 || !runners[0].Busy {
		t.Errorf("ListRunners = %+v, %v", runners, err)
	}

	if err := c.DeleteRunner(ctx, 42, "acme/api", 9); err != nil {
		t.Errorf("DeleteRunner: %v", err)
	}
	if err := c.DeleteRunner(ctx, 42, "acme/api", 404); err != nil {
		t.Errorf("DeleteRunner 404 should be ok: %v", err)
	}
	if err := c.DeleteRunner(ctx, 42, "acme/api", 500); err == nil {
		t.Error("DeleteRunner 500 should fail")
	}
	if n := tokenCalls.Load(); n != 1 {
		t.Errorf("installation token fetched %d times, want 1 (cached)", n)
	}
}
