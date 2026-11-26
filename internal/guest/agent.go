package guest

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ExitAgentError is the exit code reported when the agent itself failed
// (bad payload, a disk that never appeared, a runner that would not start).
const ExitAgentError = 254

// Layout is where things live inside the guest. Paths named "inside Root"
// are as the runner sees them after the chroot.
type Layout struct {
	// BaseRoot is the read-only base image (virtio-pmem, mounted by the
	// image's init): the overlay's lower layer.
	BaseRoot string
	// JobMount is where the job disk is mounted; the overlay's upper/ and
	// work/ directories live on it.
	JobMount string
	// Root is the merged overlay the runner and warm steps are chrooted into.
	Root string
	// Binds are guest directories bind-mounted into Root (/proc, /sys, /dev).
	Binds []string
	// Home is the runner user's home, inside Root. "~/" paths resolve here.
	Home     string
	UID, GID int
	// Dockerd is the Docker daemon started for each job, inside Root
	// ("" = none). The base image's own dockerd can't serve jobs: its
	// /var/lib/docker is on the read-only root, not the job's volume.
	Dockerd string
	// RunnerCmd is the GitHub runner's run.sh, inside Root.
	RunnerCmd string
	// Workspace is where prebuild warm steps run and where jobs find the
	// prebuild's workspace paths, inside Root.
	Workspace string
	// WarmStash is where workspace warm paths are moved before the runner
	// starts, for tachyon/warm to move back after actions/checkout. Must be
	// on the same filesystem as the workspace (a rename, not a copy).
	WarmStash string
	Shell     string
}

// DefaultLayout matches the Tachyon guest image.
func DefaultLayout() Layout {
	return Layout{
		BaseRoot:  "/run/tachyon/base",
		JobMount:  "/run/tachyon/job",
		Root:      "/run/tachyon/root",
		Binds:     []string{"/proc", "/sys", "/dev"},
		Home:      "/home/runner",
		UID:       1001,
		GID:       1001,
		Dockerd:   "/usr/bin/dockerd",
		RunnerCmd: "/home/runner/actions-runner/run.sh",
		Workspace: "/home/runner/work/_tachyon/workspace",
		WarmStash: "/opt/tachyon/warm",
		Shell:     "/bin/bash",
	}
}

const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/snap/bin"

// Agent runs one job (or one prebuild build) per VM.
type Agent struct {
	Sys    System
	Layout Layout
	Log    *slog.Logger
	// Output receives runner and warm-step output (the serial console).
	Output io.Writer
	Now    func() time.Time
}

// quiesceTimeout bounds how long Serve waits for the job's processes to
// die and its filesystems to unmount before reporting.
const quiesceTimeout = 30 * time.Second

// dockerTimeout bounds the wait for dockerd's socket. A job that doesn't use
// Docker still runs if it never appears.
const dockerTimeout = 30 * time.Second

// Serve reads one payload from conn, runs it, and writes the result back.
// The caller powers the VM off afterwards.
//
// hostd takes the result as the end of the job: it destroys the VM and
// snapshots the job disk and sticky volumes right away. So before writing
// the result Serve stops whatever the job left running and flushes and
// unmounts (or freezes) the job's filesystems. If that fails the result
// reports ExitAgentError, so hostd keeps no snapshot of the disks.
func (a *Agent) Serve(ctx context.Context, conn io.ReadWriter) error {
	p, err := ReadPayload(conn)
	if err != nil {
		a.log().Error("read payload", "err", err)
		_ = WriteResult(conn, Result{ExitCode: ExitAgentError})
		return err
	}
	res := a.Run(ctx, p)
	if err := a.quiesce(ctx); err != nil {
		a.log().Error("quiesce disks", "job", p.JobID, "err", err)
		res.ExitCode = ExitAgentError
	}
	if err := WriteResult(conn, res); err != nil {
		return fmt.Errorf("write result: %w", err)
	}
	return nil
}

// Run sets up the guest from the payload and runs the runner, or the warm
// steps for a prebuild build. It never returns the runner config in errors
// or logs.
func (a *Agent) Run(ctx context.Context, p *Payload) Result {
	res := Result{Phases: map[string]string{}}
	a.log().Info("payload received", "payload", p.String())
	code, err := a.run(ctx, p, &res)
	if err != nil {
		a.log().Error("job failed in agent", "job", p.JobID, "err", err)
		if code == 0 {
			code = ExitAgentError
		}
	}
	res.ExitCode = code
	a.log().Info("job done", "job", p.JobID, "exit_code", code)
	return res
}

func (a *Agent) run(ctx context.Context, p *Payload, res *Result) (int, error) {
	if err := a.mountDisks(ctx, p.Disks); err != nil {
		return 0, fmt.Errorf("mount disks: %w", err)
	}
	if err := a.writeFiles(p.Files); err != nil {
		return 0, fmt.Errorf("write files: %w", err)
	}
	env := a.env(p.Env)
	a.startDocker(ctx)
	if p.Prebuild != nil {
		return a.prebuild(ctx, p, env, res)
	}
	if err := a.stashWarmPaths(p.Env); err != nil {
		return 0, fmt.Errorf("stash warm paths: %w", err)
	}
	return a.runRunner(ctx, p, env, res)
}

// startDocker starts dockerd as root inside the job's root, where
// /var/lib/docker is the job's docker volume, and waits for its socket.
// Quiesce stops it with everything else the job left running.
func (a *Agent) startDocker(ctx context.Context) {
	l := a.Layout
	if l.Dockerd == "" {
		return
	}
	if _, err := os.Stat(a.host(l.Dockerd)); err != nil {
		return
	}
	// A prebuild clone carries the socket and pid file of the daemon that
	// quiesce killed; the socket would pass the readiness check below.
	sock := a.host("/run/docker.sock")
	for _, f := range []string{sock, a.host("/run/docker.pid")} {
		if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
			a.log().Warn("remove stale docker state", "path", f, "err", err)
		}
	}
	go func() {
		code, err := a.Sys.Run(ctx, Command{
			Path: l.Dockerd,
			// Steps run with no supplementary groups, so the socket's
			// group must be the runner's own.
			Args:   []string{"--group", strconv.Itoa(l.GID)},
			Env:    []string{"PATH=" + defaultPath},
			Dir:    "/",
			Root:   l.Root,
			Stdout: a.output(),
			Stderr: a.output(),
		})
		a.log().Info("dockerd exited", "code", code, "err", err)
	}()
	deadline := time.Now().Add(dockerTimeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	a.log().Warn("dockerd socket did not appear", "timeout", dockerTimeout)
}

func (a *Agent) quiesce(ctx context.Context) error {
	// Runs even if ctx was cancelled (SIGTERM): the disks still need it.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), quiesceTimeout)
	defer cancel()
	return a.Sys.Quiesce(ctx, []string{a.Layout.Root, a.Layout.JobMount})
}

func (a *Agent) log() *slog.Logger {
	if a.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return a.Log
}

func (a *Agent) now() time.Time {
	if a.Now == nil {
		return time.Now()
	}
	return a.Now()
}

func (a *Agent) output() io.Writer {
	if a.Output == nil {
		return io.Discard
	}
	return a.Output
}

// host returns where a path inside Root lives in the agent's own namespace.
func (a *Agent) host(inRoot string) string {
	return filepath.Join(a.Layout.Root, filepath.FromSlash(path.Clean("/"+inRoot)))
}

// guestPath resolves "~/x" against Home and requires an absolute path
// otherwise. With relHome, a bare relative path is also taken from Home.
func (a *Agent) guestPath(p string, relHome bool) (string, error) {
	switch {
	case p == "~":
		return a.Layout.Home, nil
	case strings.HasPrefix(p, "~/"):
		p = path.Join(a.Layout.Home, p[2:])
	case path.IsAbs(p):
	case relHome && p != "":
		p = path.Join(a.Layout.Home, p)
	default:
		return "", fmt.Errorf("path %q must be absolute or start with ~/", p)
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return "", fmt.Errorf("path %q contains ..", p)
		}
	}
	return path.Clean(p), nil
}

func (a *Agent) underHome(p string) bool {
	return p == a.Layout.Home || strings.HasPrefix(p, a.Layout.Home+"/")
}

// mkdirAll creates a directory inside Root. Directories it creates are
// handed to the runner user when they are under Home or owned is set.
func (a *Agent) mkdirAll(inRoot string, owned bool) error {
	var missing []string
	for d := inRoot; d != "/" && d != "."; d = path.Dir(d) {
		if _, err := os.Stat(a.host(d)); err == nil {
			break
		}
		missing = append(missing, d)
	}
	if err := os.MkdirAll(a.host(inRoot), 0o755); err != nil {
		return err
	}
	if !owned && !a.underHome(inRoot) {
		return nil
	}
	for _, d := range missing {
		if err := a.Sys.Chown(a.host(d), a.Layout.UID, a.Layout.GID); err != nil {
			return err
		}
	}
	return nil
}

func (a *Agent) mountDisks(ctx context.Context, disks []Disk) error {
	var root *Disk
	var sticky []Disk
	for i := range disks {
		if disks[i].Mount == "/" {
			if root != nil {
				return errors.New("more than one disk for /")
			}
			root = &disks[i]
		} else {
			sticky = append(sticky, disks[i])
		}
	}
	if root == nil {
		return errors.New("no job disk (mount /)")
	}

	// Job disk: the overlay's upper layer over the read-only base image.
	dev, err := a.attach(ctx, root.Serial)
	if err != nil {
		return err
	}
	l := a.Layout
	if err := os.MkdirAll(l.JobMount, 0o755); err != nil {
		return err
	}
	if err := a.Sys.Mount(dev, l.JobMount, "ext4", ""); err != nil {
		return fmt.Errorf("mount job disk: %w", err)
	}
	upper, work := filepath.Join(l.JobMount, "upper"), filepath.Join(l.JobMount, "work")
	for _, d := range []string{upper, work, l.Root} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", l.BaseRoot, upper, work)
	if err := a.Sys.Mount("overlay", l.Root, "overlay", opts); err != nil {
		return fmt.Errorf("mount overlay: %w", err)
	}
	for _, b := range l.Binds {
		if err := os.MkdirAll(a.host(b), 0o755); err != nil {
			return err
		}
		if err := a.Sys.Mount(b, a.host(b), "bind", ""); err != nil {
			return fmt.Errorf("bind %s: %w", b, err)
		}
	}

	// Sticky volumes, at their mounts inside the overlay.
	for _, d := range sticky {
		target, err := a.guestPath(d.Mount, false)
		if err != nil {
			return fmt.Errorf("volume %s: %w", d.Serial, err)
		}
		if target == "/" {
			return fmt.Errorf("volume %s: cannot mount at /", d.Serial)
		}
		dev, err := a.attach(ctx, d.Serial)
		if err != nil {
			return err
		}
		if err := a.mkdirAll(target, false); err != nil {
			return err
		}
		if err := a.Sys.Mount(dev, a.host(target), "ext4", ""); err != nil {
			return fmt.Errorf("mount volume %s at %s: %w", d.Serial, target, err)
		}
		if a.underHome(target) {
			// A fresh filesystem's root is owned by root.
			if err := a.Sys.Chown(a.host(target), l.UID, l.GID); err != nil {
				return err
			}
		}
		a.log().Info("volume mounted", "serial", d.Serial, "mount", target)
	}
	return nil
}

func (a *Agent) attach(ctx context.Context, serial string) (string, error) {
	dev, err := a.Sys.DiskBySerial(ctx, serial)
	if err != nil {
		return "", fmt.Errorf("disk %s: %w", serial, err)
	}
	if err := a.Sys.EnsureFilesystem(ctx, dev); err != nil {
		return "", fmt.Errorf("disk %s: %w", serial, err)
	}
	return dev, nil
}

func (a *Agent) writeFiles(files map[string]string) error {
	for _, name := range sortedKeys(files) {
		p, err := a.guestPath(name, false)
		if err != nil {
			return err
		}
		body, err := base64.StdEncoding.DecodeString(files[name])
		if err != nil {
			return fmt.Errorf("%s: contents are not base64", p)
		}
		if err := a.mkdirAll(path.Dir(p), false); err != nil {
			return err
		}
		if err := os.WriteFile(a.host(p), body, 0o644); err != nil {
			return err
		}
		if a.underHome(p) {
			if err := a.Sys.Chown(a.host(p), a.Layout.UID, a.Layout.GID); err != nil {
				return err
			}
		}
	}
	return nil
}

// env is the environment for the runner and warm steps. It never includes
// the runner config.
func (a *Agent) env(extra map[string]string) []string {
	m := map[string]string{
		"HOME":               a.Layout.Home,
		"USER":               "runner",
		"LANG":               "C.UTF-8",
		"PATH":               defaultPath,
		"TACHYON_WARM_STASH": a.Layout.WarmStash,
	}
	for k, v := range extra {
		m[k] = v
	}
	out := make([]string, 0, len(m))
	for _, k := range sortedKeys(m) {
		out = append(out, k+"="+m[k])
	}
	return out
}

// stashWarmPaths moves the prebuild's workspace paths out of the workspace
// before the runner starts, because actions/checkout cleans untracked files.
// tachyon/warm moves them back.
func (a *Agent) stashWarmPaths(env map[string]string) error {
	if env["TACHYON_PREBUILD_MATCH"] != "full" || env["TACHYON_WARM_PATHS"] == "" {
		return nil
	}
	for _, p := range strings.Split(env["TACHYON_WARM_PATHS"], ":") {
		if p == "" || path.IsAbs(p) || strings.Contains(p, "..") {
			a.log().Warn("skipping unsafe warm path", "path", p)
			continue
		}
		p = path.Clean(p)
		src := path.Join(a.Layout.Workspace, p)
		if _, err := os.Lstat(a.host(src)); err != nil {
			a.log().Warn("warm path not in prebuild", "path", p)
			continue
		}
		dst := path.Join(a.Layout.WarmStash, p)
		if err := a.mkdirAll(path.Dir(dst), true); err != nil {
			return err
		}
		if err := os.RemoveAll(a.host(dst)); err != nil {
			return err
		}
		if err := os.Rename(a.host(src), a.host(dst)); err != nil {
			return err
		}
	}
	// The runner moves entries out of the stash, so it must own the top too.
	if _, err := os.Stat(a.host(a.Layout.WarmStash)); err == nil {
		return a.Sys.Chown(a.host(a.Layout.WarmStash), a.Layout.UID, a.Layout.GID)
	}
	return nil
}

func (a *Agent) runRunner(ctx context.Context, p *Payload, env []string, res *Result) (int, error) {
	cfg, err := p.RunnerConfig()
	if err != nil {
		return 0, err
	}
	if len(cfg) == 0 {
		return 0, errors.New("no runner config")
	}
	// The JIT config goes to the runner on its command line only; it is
	// never written to the guest disk (which may become a sticky volume or
	// be inspected after the job).
	w := newPhaseWatcher(a.output(), a.now)
	code, err := a.Sys.Run(ctx, Command{
		Path:   a.Layout.RunnerCmd,
		Args:   []string{"--jitconfig", string(cfg)},
		Env:    env,
		Dir:    a.Layout.Home,
		Root:   a.Layout.Root,
		UID:    a.Layout.UID,
		GID:    a.Layout.GID,
		Stdout: w,
		Stderr: w,
	})
	w.Flush()
	for k, v := range w.Phases() {
		res.Phases[k] = v
	}
	if err != nil {
		return 0, fmt.Errorf("start runner: %w", err)
	}
	switch r := w.Result(); {
	case r == "Succeeded":
		return 0, nil
	case r != "":
		return 1, nil
	case code != 0:
		return code, nil
	default:
		// The runner exited cleanly without finishing a job.
		return 1, errors.New("runner exited without completing a job")
	}
}

// prebuild runs the warm steps, scrubs credentials and scans the writable
// layer. It never starts a runner.
func (a *Agent) prebuild(ctx context.Context, p *Payload, env []string, res *Result) (int, error) {
	pb := p.Prebuild
	l := a.Layout
	if err := a.mkdirAll(l.Workspace, true); err != nil {
		return 0, err
	}
	code := 0
	step := func(stepEnv []string, c string) (int, error) {
		return a.Sys.Run(ctx, Command{
			Path:   l.Shell,
			Args:   []string{"-euo", "pipefail", "-c", c},
			Env:    stepEnv,
			Dir:    l.Workspace,
			Root:   l.Root,
			UID:    l.UID,
			GID:    l.GID,
			Stdout: a.output(),
			Stderr: a.output(),
		})
	}
	// The clone token is visible to the clone commands only.
	var cloneToken []byte
	if len(pb.CloneCommands) > 0 {
		tok, err := base64.StdEncoding.DecodeString(pb.CloneTokenB64)
		if err != nil {
			return 0, errors.New("decode clone token")
		}
		cloneToken = tok
		cloneEnv := append(append([]string(nil), env...), "TACHYON_CLONE_TOKEN="+string(tok))
		for i, c := range pb.CloneCommands {
			a.log().Info("clone step", "step", i+1, "of", len(pb.CloneCommands), "refresh", pb.Refresh)
			exit, err := step(cloneEnv, c)
			if err != nil {
				return 0, fmt.Errorf("clone step %d: %w", i+1, err)
			}
			if exit != 0 {
				a.log().Error("clone step failed", "step", i+1, "exit_code", exit)
				code = exit
				break
			}
		}
	}
	for i, c := range pb.WarmCommands {
		if code != 0 {
			break // the clone failed; nothing to warm
		}
		a.log().Info("warm step", "step", i+1, "of", len(pb.WarmCommands))
		exit, err := step(env, c)
		if err != nil {
			return 0, fmt.Errorf("warm step %d: %w", i+1, err)
		}
		if exit != 0 {
			a.log().Error("warm step failed", "step", i+1, "exit_code", exit)
			code = exit
			break
		}
	}
	for _, kp := range pb.KeepPaths {
		gp, err := a.guestPath(kp, false)
		if err != nil {
			// Relative keep paths are inside the workspace.
			gp = path.Join(l.Workspace, path.Clean(kp))
		}
		if _, err := os.Lstat(a.host(gp)); err != nil {
			a.log().Warn("prebuild path missing after warm steps", "path", kp)
		}
	}

	// Scrub known credential files, then scan whatever is left in the
	// writable layer. Removing through the overlay leaves whiteouts, so the
	// files are gone from the snapshot too.
	for _, s := range pb.ScrubPaths {
		gp, err := a.guestPath(s, true)
		if err != nil {
			return 0, fmt.Errorf("scrub: %w", err)
		}
		if err := os.RemoveAll(a.host(gp)); err != nil {
			return 0, fmt.Errorf("scrub %s: %w", gp, err)
		}
	}
	var literals [][]byte
	if cfg, err := p.RunnerConfig(); err == nil && len(cfg) > 0 {
		literals = append(literals, cfg)
	}
	if len(cloneToken) > 0 {
		literals = append(literals, cloneToken)
	}
	findings, err := ScanSecrets(filepath.Join(l.JobMount, "upper"), literals)
	clean := err == nil && len(findings) == 0
	if err != nil {
		a.log().Error("secret scan", "err", err)
	}
	for _, f := range findings {
		// Never log the match itself.
		a.log().Error("secret found in prebuild", "path", f.Path, "rule", f.Rule)
	}
	res.SecretScanClean = &clean
	return code, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
