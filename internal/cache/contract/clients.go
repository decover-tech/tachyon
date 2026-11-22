package contract

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Client saves and restores cache entries the way one GitHub client does.
// paths are relative to the workspace, as in a workflow.
type Client interface {
	Name() string
	Save(ctx context.Context, job JobSpec, workspace, key string, paths []string) (StepResult, error)
	Restore(ctx context.Context, job JobSpec, workspace, key string, restoreKeys, paths []string) (StepResult, error)
}

// ActionClient runs a checkout of actions/cache: its save-only and
// restore-only entry points (actions/cache/save and actions/cache/restore),
// with the inputs and environment the runner would give them.
type ActionClient struct {
	Dir     string // a checkout of github.com/actions/cache at a release tag
	Version string // the tag, for reports
	Runner  Runner
	Target  Target
}

func (c *ActionClient) Name() string { return label("actions/cache", c.Version) }

func (c *ActionClient) script(name string) (string, error) {
	p := filepath.Join(c.Dir, "dist", name, "index.js")
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("actions/cache checkout: %w", err)
	}
	return p, nil
}

func (c *ActionClient) Save(ctx context.Context, job JobSpec, workspace, key string, paths []string) (StepResult, error) {
	script, err := c.script("save-only")
	if err != nil {
		return StepResult{}, err
	}
	return runStep(ctx, c.Runner, c.Target, job, workspace, script, map[string]string{
		"path":                 strings.Join(paths, "\n"),
		"key":                  key,
		"upload-chunk-size":    "",
		"enableCrossOsArchive": "false",
	})
}

func (c *ActionClient) Restore(ctx context.Context, job JobSpec, workspace, key string, restoreKeys, paths []string) (StepResult, error) {
	script, err := c.script("restore-only")
	if err != nil {
		return StepResult{}, err
	}
	return runStep(ctx, c.Runner, c.Target, job, workspace, script, map[string]string{
		"path":                 strings.Join(paths, "\n"),
		"key":                  key,
		"restore-keys":         strings.Join(restoreKeys, "\n"),
		"enableCrossOsArchive": "false",
		"fail-on-cache-miss":   "false",
		"lookup-only":          "false",
	})
}

//go:embed toolkit/driver.mjs
var toolkitDriver []byte

// driverName is where ToolkitClient writes the driver inside Dir, so its
// bare import of @actions/cache resolves from Dir/node_modules.
const driverName = "tachyon-contract-driver.mjs"

// ToolkitClient calls @actions/cache (the toolkit package that
// actions/cache and the setup-* actions bundle) directly, through a small
// driver script.
type ToolkitClient struct {
	Dir    string // a directory where `npm install @actions/cache` was run
	Runner Runner
	Target Target
}

func (c *ToolkitClient) Name() string { return label("@actions/cache", c.Version()) }

// Version is the installed toolkit's version, or "" if unknown.
func (c *ToolkitClient) Version() string {
	b, err := os.ReadFile(filepath.Join(c.Dir, "node_modules", "@actions", "cache", "package.json"))
	if err != nil {
		return ""
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(b, &pkg) != nil {
		return ""
	}
	return pkg.Version
}

func (c *ToolkitClient) driver() (string, error) {
	p := filepath.Join(c.Dir, driverName)
	if err := os.WriteFile(p, toolkitDriver, 0o644); err != nil {
		return "", err
	}
	return p, nil
}

func (c *ToolkitClient) Save(ctx context.Context, job JobSpec, workspace, key string, paths []string) (StepResult, error) {
	d, err := c.driver()
	if err != nil {
		return StepResult{}, err
	}
	return runStep(ctx, c.Runner, c.Target, job, workspace, d, nil, "save", strings.Join(paths, "\n"), key)
}

func (c *ToolkitClient) Restore(ctx context.Context, job JobSpec, workspace, key string, restoreKeys, paths []string) (StepResult, error) {
	d, err := c.driver()
	if err != nil {
		return StepResult{}, err
	}
	args := append([]string{"restore", strings.Join(paths, "\n"), key}, restoreKeys...)
	return runStep(ctx, c.Runner, c.Target, job, workspace, d, nil, args...)
}

func label(name, version string) string {
	if version == "" {
		return name
	}
	return name + "@" + version
}
