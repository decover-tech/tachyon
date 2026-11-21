package mirrors

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestEnv(t *testing.T) {
	e := Endpoints{NPM: "https://npm.m", PyPI: "https://pypi.m/simple", GoProxy: "https://go.m"}
	want := map[string]string{
		"npm_config_registry": "https://npm.m",
		"PIP_INDEX_URL":       "https://pypi.m/simple",
		"GOPROXY":             "https://go.m,direct",
	}
	if got := e.Env(); !reflect.DeepEqual(got, want) {
		t.Errorf("Env = %v", got)
	}
	if got := (Endpoints{}).Env(); len(got) != 0 {
		t.Errorf("empty endpoints Env = %v", got)
	}
}

func TestDockerDaemonConfig(t *testing.T) {
	base := map[string]any{"log-driver": "json-file", "registry-mirrors": []string{"stale"}}
	b, err := Endpoints{Registry: "https://reg.m"}.DockerDaemonConfig(base)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	json.Unmarshal(b, &got)
	if got["log-driver"] != "json-file" || !reflect.DeepEqual(got["registry-mirrors"], []any{"https://reg.m"}) {
		t.Errorf("config = %s", b)
	}
	if _, ok := base["registry-mirrors"].([]string); !ok {
		t.Error("base map mutated")
	}
	b, _ = Endpoints{}.DockerDaemonConfig(base)
	got = nil
	json.Unmarshal(b, &got)
	if _, ok := got["registry-mirrors"]; ok {
		t.Error("no registry mirror should remove registry-mirrors (upstream fallback)")
	}
}

func TestPairPick(t *testing.T) {
	tests := []struct {
		p    Pair
		want string
	}{
		{Pair{"a", "b", true, true}, "a"},
		{Pair{"a", "b", false, true}, "b"},
		{Pair{"a", "b", false, false}, ""},
	}
	for _, tt := range tests {
		if got := tt.p.Pick(); got != tt.want {
			t.Errorf("%+v Pick = %q", tt.p, got)
		}
	}
}

func TestGitConfig(t *testing.T) {
	e := Endpoints{Git: "https://git.m/"}
	got := e.GitConfig([]string{"zeta/b", "acme/api"})
	want := `[url "https://git.m/acme/api"]
	insteadOf = https://github.com/acme/api
[url "https://github.com/acme/api"]
	pushInsteadOf = https://github.com/acme/api
[url "https://git.m/zeta/b"]
	insteadOf = https://github.com/zeta/b
[url "https://github.com/zeta/b"]
	pushInsteadOf = https://github.com/zeta/b
`
	if got != want {
		t.Errorf("GitConfig =\n%s\nwant\n%s", got, want)
	}
	if (Endpoints{}).GitConfig([]string{"a/b"}) != "" || e.GitConfig(nil) != "" {
		t.Error("expected empty config")
	}
}

func TestOtherConfigs(t *testing.T) {
	e := Endpoints{Apt: "http://apt.m:3142", Maven: "https://maven.m"}
	if got := e.AptConf(); got != "Acquire::http::Proxy \"http://apt.m:3142\";\n" {
		t.Errorf("AptConf = %q", got)
	}
	if !strings.Contains(e.GradleInit(), `"https://maven.m"`) {
		t.Errorf("GradleInit = %s", e.GradleInit())
	}
	if !strings.Contains(e.MavenSettings(), "<url>https://maven.m</url>") {
		t.Errorf("MavenSettings = %s", e.MavenSettings())
	}
	var none Endpoints
	if none.AptConf() != "" || none.GradleInit() != "" || none.MavenSettings() != "" {
		t.Error("empty endpoints should produce no config")
	}
}
