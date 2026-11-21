package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/decover-tech/tachyon/internal/jobapi"
)

func TestParseLabels(t *testing.T) {
	tests := []struct {
		name    string
		labels  []string
		want    RunsOn
		wantErr bool
	}{
		{"8x", []string{"tachyon-ubuntu-24.04-8x"}, RunsOn{Image: "ubuntu-24.04", Hardware: jobapi.NewHardware(jobapi.ArchX86_64, 8)}, false},
		{"arm", []string{"tachyon-ubuntu-24.04-arm-4x"}, RunsOn{Image: "ubuntu-24.04", Hardware: jobapi.NewHardware(jobapi.ArchARM64, 4)}, false},
		{"default size", []string{"tachyon-ubuntu-24.04"}, RunsOn{Image: "ubuntu-24.04", Hardware: jobapi.NewHardware(jobapi.ArchX86_64, 2)}, false},
		{"modifiers and other labels", []string{"self-hosted", "tachyon-clean", "tachyon-ubuntu-24.04-16x", "tachyon-deferrable"},
			RunsOn{Image: "ubuntu-24.04", Hardware: jobapi.NewHardware(jobapi.ArchX86_64, 16), Clean: true, Deferrable: true}, false},
		{"unknown label", []string{"tachyon-ubuntu-24.04-8x", "tachyon-turbo"}, RunsOn{}, true},
		{"two runner labels", []string{"tachyon-ubuntu-24.04-8x", "tachyon-ubuntu-24.04-4x"}, RunsOn{}, true},
		{"bad size", []string{"tachyon-ubuntu-24.04-3x"}, RunsOn{}, true},
		{"no runner label", []string{"tachyon-clean"}, RunsOn{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseLabels(tt.labels)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestIsTachyonJob(t *testing.T) {
	if IsTachyonJob([]string{"ubuntu-latest"}) {
		t.Error("ubuntu-latest is not a Tachyon job")
	}
	if !IsTachyonJob([]string{"self-hosted", "tachyon-ubuntu-24.04-2x"}) {
		t.Error("expected Tachyon job")
	}
}

func TestDeriveTrust(t *testing.T) {
	base := "acme/api"
	tests := []struct {
		name string
		rc   RunContext
		want jobapi.Trust
	}{
		{"push default", RunContext{Event: "push", HeadBranch: "main", HeadRepo: base, BaseRepo: base, DefaultBranch: "main"}, jobapi.Trusted},
		{"push protected", RunContext{Event: "push", HeadBranch: "release", HeadRepo: base, BaseRepo: base, DefaultBranch: "main", Protected: true}, jobapi.Trusted},
		{"push feature", RunContext{Event: "push", HeadBranch: "feat", HeadRepo: base, BaseRepo: base, DefaultBranch: "main"}, jobapi.Untrusted},
		{"same-repo PR", RunContext{Event: "pull_request", HeadBranch: "feat", HeadRepo: base, BaseRepo: base, DefaultBranch: "main"}, jobapi.Untrusted},
		{"same-repo PR from main branch name", RunContext{Event: "pull_request", HeadBranch: "main", HeadRepo: base, BaseRepo: base, DefaultBranch: "main"}, jobapi.Untrusted},
		{"fork PR", RunContext{Event: "pull_request", HeadBranch: "main", HeadRepo: "evil/api", BaseRepo: base, DefaultBranch: "main"}, jobapi.UntrustedFork},
		{"pull_request_target fork", RunContext{Event: "pull_request_target", HeadBranch: "main", HeadRepo: "evil/api", BaseRepo: base, DefaultBranch: "main"}, jobapi.UntrustedFork},
		{"schedule default", RunContext{Event: "schedule", HeadBranch: "main", HeadRepo: base, BaseRepo: base, DefaultBranch: "main"}, jobapi.Trusted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DeriveTrust(tt.rc); got != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func sign(secret, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func TestVerifySignature(t *testing.T) {
	secret, body := []byte("s3cret"), []byte(`{"a":1}`)
	good := sign(secret, body)
	tests := []struct {
		name   string
		secret []byte
		header string
		want   bool
	}{
		{"good", secret, good, true},
		{"wrong secret", []byte("other"), good, false},
		{"no prefix", secret, good[len("sha256="):], false},
		{"not hex", secret, "sha256=zz", false},
		{"empty secret", nil, good, false},
		{"empty header", secret, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := VerifySignature(tt.secret, body, tt.header); got != tt.want {
				t.Fatalf("got %v want %v", got, tt.want)
			}
		})
	}
}
