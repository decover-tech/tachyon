package images

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is images/<name>/image.yml: which upstream runner-images template
// an image follows, and what Tachyon trims from it.
type Config struct {
	Name         string       `yaml:"name"`
	Arch         string       `yaml:"arch"`
	RunnerImages RunnerImages `yaml:"runner_images"`
	// BaseRootfs is the Ubuntu cloud image root filesystem the build starts
	// from, as GitHub's image starts from a cloud VM image. Pinned by digest.
	BaseRootfs BaseRootfs `yaml:"base_rootfs"`
	// RootfsSizeGB is the ext4 file's size.
	RootfsSizeGB int `yaml:"rootfs_size_gb"`
	// Skip lists upstream build scripts Tachyon leaves out, each with a
	// reason. Every entry must still exist upstream, so a rename upstream
	// fails the build instead of silently un-trimming the image.
	Skip []Skip `yaml:"skip"`
	// Vars are the values of ${var.*} in the template's environment_vars.
	// image_version and apt_snapshot_timestamp are set per build.
	Vars map[string]string `yaml:"vars"`
}

type RunnerImages struct {
	Repo     string `yaml:"repo"`
	Commit   string `yaml:"commit"`
	Template string `yaml:"template"`
	Toolset  string `yaml:"toolset"`
}

type BaseRootfs struct {
	URL    string `yaml:"url"`
	SHA256 string `yaml:"sha256"`
}

type Skip struct {
	Script string `yaml:"script"`
	Reason string `yaml:"reason"`
}

var commitRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ParseConfig decodes and validates image.yml. Unknown fields are rejected.
func ParseConfig(data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("image.yml: %w", err)
	}
	if !nameRe.MatchString(c.Name) {
		return nil, fmt.Errorf("image.yml: name %q is invalid", c.Name)
	}
	if c.Arch != "x86_64" && c.Arch != "arm64" {
		return nil, fmt.Errorf("image.yml: arch %q: want x86_64 or arm64", c.Arch)
	}
	ri := c.RunnerImages
	if ri.Repo == "" || ri.Template == "" || ri.Toolset == "" {
		return nil, errors.New("image.yml: runner_images.repo, template and toolset are required")
	}
	if !commitRe.MatchString(ri.Commit) {
		return nil, fmt.Errorf("image.yml: runner_images.commit %q must be a full commit sha", ri.Commit)
	}
	if !strings.HasPrefix(c.BaseRootfs.URL, "https://") || !hexRe.MatchString(c.BaseRootfs.SHA256) {
		return nil, errors.New("image.yml: base_rootfs needs an https url and its sha256")
	}
	if c.RootfsSizeGB < 8 {
		return nil, errors.New("image.yml: rootfs_size_gb must be at least 8")
	}
	seen := map[string]bool{}
	for _, s := range c.Skip {
		if s.Script == "" || s.Reason == "" {
			return nil, errors.New("image.yml: every skip entry needs a script and a reason")
		}
		if seen[s.Script] {
			return nil, fmt.Errorf("image.yml: %s skipped twice", s.Script)
		}
		seen[s.Script] = true
	}
	return &c, nil
}

// Step is one upstream build script, run inside the image root.
type Step struct {
	Script string   `json:"script"` // file name under images/ubuntu/scripts/build
	Shell  string   `json:"shell"`  // sh | pwsh
	Root   bool     `json:"root"`   // upstream runs it under sudo
	Env    []string `json:"env"`    // KEY=VALUE, in template order
}

// Plan is the ordered list of steps a build runs. Its digest goes into the
// manifest, so two builds can be compared input for input.
type Plan struct {
	Image   string   `json:"image"`
	Commit  string   `json:"runner_images_commit"`
	Steps   []Step   `json:"steps"`
	Skipped []string `json:"skipped"`
}

// BuildVars are the per-build template variables.
type BuildVars struct {
	ImageVersion string
	APTSnapshot  string // e.g. 20261116T000000Z
}

// MakePlan reads the upstream Packer template and applies the config's
// trims. Only shell provisioners that run scripts/build/* become steps;
// upstream's file copies and inline commands are done by build.sh.
func MakePlan(cfg *Config, template []byte, bv BuildVars) (*Plan, error) {
	vars := map[string]string{}
	for k, v := range cfg.Vars {
		vars[k] = v
	}
	vars["image_version"] = bv.ImageVersion
	vars["apt_snapshot_timestamp"] = bv.APTSnapshot

	steps, err := parseTemplate(string(template), vars)
	if err != nil {
		return nil, err
	}
	skip := map[string]bool{}
	for _, s := range cfg.Skip {
		skip[s.Script] = true
	}
	p := &Plan{Image: cfg.Name, Commit: cfg.RunnerImages.Commit}
	found := map[string]bool{}
	for _, s := range steps {
		found[s.Script] = true
		if skip[s.Script] {
			p.Skipped = append(p.Skipped, s.Script)
			continue
		}
		p.Steps = append(p.Steps, s)
	}
	var stale []string
	for s := range skip {
		if !found[s] {
			stale = append(stale, s)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		return nil, fmt.Errorf("skip entries not in the upstream template (renamed or removed upstream?): %s", strings.Join(stale, ", "))
	}
	if len(p.Steps) == 0 {
		return nil, errors.New("plan has no steps")
	}
	return p, nil
}

// Digest is the SHA-256 of the plan's JSON encoding.
func (p *Plan) Digest() string {
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Shell renders the plan as calls to a run_step function that build.sh
// defines: run_step <root|user> <sh|pwsh> <script> [KEY=VALUE...].
func (p *Plan) Shell() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Generated by tachyon-image plan from actions/runner-images@%s. Do not edit.\n", p.Commit)
	fmt.Fprintf(&b, "# plan sha256 %s\n", p.Digest())
	for _, s := range p.Skipped {
		fmt.Fprintf(&b, "# skipped: %s\n", s)
	}
	for _, s := range p.Steps {
		who := "user"
		if s.Root {
			who = "root"
		}
		fmt.Fprintf(&b, "run_step %s %s %s", who, s.Shell, shellQuote(s.Script))
		for _, e := range s.Env {
			b.WriteString(" " + shellQuote(e))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

var (
	quotedRe = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)
	varRe    = regexp.MustCompile(`\$\{var\.([a-zA-Z0-9_]+)\}`)
)

// parseTemplate extracts the scripts/build steps from a Packer HCL template,
// in order. It understands the subset of HCL upstream uses for shell
// provisioners: execute_command, environment_vars, script and scripts.
func parseTemplate(src string, vars map[string]string) ([]Step, error) {
	var steps []Step
	const marker = `provisioner "shell"`
	for {
		i := strings.Index(src, marker)
		if i < 0 {
			break
		}
		src = src[i+len(marker):]
		open := strings.IndexByte(src, '{')
		if open < 0 {
			return nil, errors.New("template: provisioner without a body")
		}
		end, err := matchBrace(src, open)
		if err != nil {
			return nil, err
		}
		body := src[open+1 : end]
		src = src[end+1:]

		exec := attrString(body, "execute_command")
		shell := "sh"
		if strings.Contains(exec, "pwsh") {
			shell = "pwsh"
		}
		root := strings.Contains(exec, "sudo")
		var env []string
		for _, e := range attrList(body, "environment_vars") {
			v, err := expand(e, vars)
			if err != nil {
				return nil, err
			}
			env = append(env, v)
		}
		scripts := attrList(body, "scripts")
		if s := attrString(body, "script"); s != "" {
			scripts = append(scripts, s)
		}
		for _, s := range scripts {
			if !strings.Contains(s, "/scripts/build/") {
				continue
			}
			steps = append(steps, Step{Script: path.Base(s), Shell: shell, Root: root, Env: env})
		}
	}
	if len(steps) == 0 {
		return nil, errors.New("template: no scripts/build provisioners found")
	}
	return steps, nil
}

// matchBrace returns the index of the brace closing src[open], skipping
// braces inside string literals ("{{ .Vars }}", "${var.x}").
func matchBrace(src string, open int) (int, error) {
	depth := 0
	inStr := false
	for i := open; i < len(src); i++ {
		c := src[i]
		switch {
		case inStr && c == '\\':
			i++
		case c == '"':
			inStr = !inStr
		case inStr:
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return i, nil
			}
		}
	}
	return 0, errors.New("template: unbalanced braces")
}

// attrString returns the value of `name = "..."` in an HCL body.
func attrString(body, name string) string {
	re := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(name) + `\s*=\s*"((?:[^"\\]|\\.)*)"`)
	if m := re.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	return ""
}

// attrList returns the strings of `name = [ "...", ... ]` in an HCL body.
func attrList(body, name string) []string {
	re := regexp.MustCompile(`(?ms)^\s*` + regexp.QuoteMeta(name) + `\s*=\s*\[(.*?)\]`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		return nil
	}
	var out []string
	for _, q := range quotedRe.FindAllStringSubmatch(m[1], -1) {
		out = append(out, q[1])
	}
	return out
}

func expand(s string, vars map[string]string) (string, error) {
	var missing []string
	out := varRe.ReplaceAllStringFunc(s, func(m string) string {
		name := varRe.FindStringSubmatch(m)[1]
		v, ok := vars[name]
		if !ok {
			missing = append(missing, name)
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("template: no value for var.%s (set it under vars in image.yml)", missing[0])
	}
	return out, nil
}

// ShellEnv renders the config as shell assignments for build.sh.
func (c *Config) ShellEnv() string {
	var b strings.Builder
	for _, kv := range [][2]string{
		{"IMAGE_NAME", c.Name},
		{"IMAGE_ARCH", c.Arch},
		{"RUNNER_IMAGES_REPO", c.RunnerImages.Repo},
		{"RUNNER_IMAGES_COMMIT", c.RunnerImages.Commit},
		{"RUNNER_IMAGES_TOOLSET", c.RunnerImages.Toolset},
		{"BASE_ROOTFS_URL", c.BaseRootfs.URL},
		{"BASE_ROOTFS_SHA256", c.BaseRootfs.SHA256},
		{"ROOTFS_SIZE_GB", fmt.Sprint(c.RootfsSizeGB)},
	} {
		fmt.Fprintf(&b, "%s=%s\n", kv[0], shellQuote(kv[1]))
	}
	return b.String()
}
