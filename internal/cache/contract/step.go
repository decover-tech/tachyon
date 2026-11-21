package contract

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Runner runs one Node process. Exec is the real one; tests use fakes.
type Runner interface {
	// Run runs `node args...` in dir with exactly env and returns its
	// combined output. A non-zero exit is an error.
	Run(ctx context.Context, dir string, env []string, args ...string) ([]byte, error)
}

// Exec runs the node binary on this machine.
type Exec struct {
	Node string // default "node"
}

func (e Exec) Run(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	node := e.Node
	if node == "" {
		node = "node"
	}
	cmd := exec.CommandContext(ctx, node, args...)
	cmd.Dir = dir
	cmd.Env = env
	return cmd.CombinedOutput()
}

// Annotation is a workflow command a step printed, such as
// "::warning::Failed to restore: ...".
type Annotation struct {
	Level   string // "warning" or "error"
	Message string
}

func (a Annotation) String() string { return a.Level + ": " + a.Message }

// StepResult is what one action or toolkit step produced.
type StepResult struct {
	Outputs     map[string]string
	Annotations []Annotation
	Log         []byte
}

// Hit reports whether a restore matched the primary key exactly.
func (r StepResult) Hit() bool { return r.Outputs["cache-hit"] == "true" }

// MatchedKey is the key a restore matched, or "" on a miss.
func (r StepResult) MatchedKey() string { return r.Outputs["cache-matched-key"] }

// Problems returns the step's warning and error annotations. A step on a
// path that should work must have none: actions/cache and the toolkit turn
// protocol errors into warnings rather than failing the step.
func (r StepResult) Problems() []Annotation {
	var out []Annotation
	for _, a := range r.Annotations {
		if a.Level == "warning" || a.Level == "error" {
			out = append(out, a)
		}
	}
	return out
}

// stepEnv is the environment the Actions runner gives a JavaScript action
// step, pointed at target for job. It is built from scratch so a contract
// run inside GitHub Actions never inherits, or leaks, the real runner's
// ACTIONS_RUNTIME_TOKEN or ACTIONS_RESULTS_URL.
func stepEnv(target Target, job JobSpec, workspace, temp, output, state string, inputs map[string]string) []string {
	env := map[string]string{
		"PATH":                     os.Getenv("PATH"),
		"HOME":                     os.Getenv("HOME"),
		"CI":                       "true",
		"GITHUB_ACTIONS":           "true",
		"GITHUB_SERVER_URL":        "https://github.com",
		"GITHUB_REPOSITORY":        target.Repo,
		"GITHUB_REF":               job.Ref,
		"GITHUB_EVENT_NAME":        job.Event,
		"GITHUB_WORKSPACE":         workspace,
		"GITHUB_OUTPUT":            output,
		"GITHUB_STATE":             state,
		"RUNNER_TEMP":              temp,
		"RUNNER_OS":                runnerOS(),
		"ACTIONS_CACHE_SERVICE_V2": "true",
		// The runner hands out the results URL with a trailing slash.
		"ACTIONS_RESULTS_URL":   strings.TrimSuffix(target.URL, "/") + "/",
		"ACTIONS_RUNTIME_TOKEN": job.Token,
	}
	for k, v := range inputs {
		env[inputEnv(k)] = v
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// inputEnv is the variable @actions/core reads an action input from.
func inputEnv(name string) string {
	return "INPUT_" + strings.ToUpper(strings.ReplaceAll(name, " ", "_"))
}

func runnerOS() string {
	switch runtime.GOOS {
	case "darwin":
		return "macOS"
	case "windows":
		return "Windows"
	default:
		return "Linux"
	}
}

// runStep runs `node script args...` as an Actions step for job and
// collects its outputs and annotations.
func runStep(ctx context.Context, r Runner, target Target, job JobSpec, workspace, script string, inputs map[string]string, args ...string) (StepResult, error) {
	// The toolkit archives paths relative to GITHUB_WORKSPACE but globs
	// them from the resolved cwd, so the two must agree (macOS's /var is a
	// symlink to /private/var).
	workspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return StepResult{}, err
	}
	temp, err := os.MkdirTemp("", "tachyon-contract-step-")
	if err != nil {
		return StepResult{}, err
	}
	defer os.RemoveAll(temp)
	output := filepath.Join(temp, "output")
	state := filepath.Join(temp, "state")
	for _, f := range []string{output, state} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			return StepResult{}, err
		}
	}
	runnerTemp := filepath.Join(temp, "runner")
	if err := os.Mkdir(runnerTemp, 0o700); err != nil {
		return StepResult{}, err
	}
	env := stepEnv(target, job, workspace, runnerTemp, output, state, inputs)
	log, runErr := r.Run(ctx, workspace, env, append([]string{script}, args...)...)
	res := StepResult{Log: log, Annotations: parseAnnotations(log)}
	raw, err := os.ReadFile(output)
	if err != nil {
		return res, err
	}
	if res.Outputs, err = parseOutputFile(raw); err != nil {
		return res, err
	}
	if runErr != nil {
		return res, fmt.Errorf("%s exited: %w", filepath.Base(filepath.Dir(script))+"/"+filepath.Base(script), runErr)
	}
	return res, nil
}

// parseOutputFile parses a $GITHUB_OUTPUT file: "name=value" lines and
// "name<<DELIM" ... "DELIM" blocks.
func parseOutputFile(b []byte) (map[string]string, error) {
	out := map[string]string{}
	lines := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if line == "" {
			continue
		}
		eq := strings.Index(line, "=")
		heredoc := strings.Index(line, "<<")
		if heredoc > 0 && (eq < 0 || heredoc < eq) {
			name, delim := line[:heredoc], line[heredoc+2:]
			if delim == "" {
				return nil, fmt.Errorf("output %q: empty delimiter", name)
			}
			var val []string
			closed := false
			for i++; i < len(lines); i++ {
				if lines[i] == delim {
					closed = true
					break
				}
				val = append(val, lines[i])
			}
			if !closed {
				return nil, fmt.Errorf("output %q: missing delimiter %q", name, delim)
			}
			out[name] = strings.Join(val, "\n")
			continue
		}
		if eq <= 0 {
			return nil, fmt.Errorf("malformed output line %q", line)
		}
		out[line[:eq]] = line[eq+1:]
	}
	return out, nil
}

// parseAnnotations finds ::warning:: and ::error:: workflow commands in a
// step's log.
func parseAnnotations(log []byte) []Annotation {
	var out []Annotation
	sc := bufio.NewScanner(bytes.NewReader(log))
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		for _, level := range []string{"warning", "error"} {
			rest, ok := strings.CutPrefix(line, "::"+level)
			if !ok || (rest != "" && rest[0] != ':' && rest[0] != ' ') {
				continue
			}
			// "::warning file=x,line=1::message" or "::warning::message"
			_, msg, ok := strings.Cut(rest, "::")
			if !ok {
				continue
			}
			out = append(out, Annotation{Level: level, Message: unescapeCommand(msg)})
		}
	}
	return out
}

func unescapeCommand(s string) string {
	return strings.NewReplacer("%0D", "\r", "%0A", "\n", "%25", "%").Replace(s)
}
