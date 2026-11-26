// Package guest is the agent that runs inside each job VM. hostd connects to
// it over vsock, sends one Payload, and waits for one Result; the agent then
// powers the VM off.
//
// The wire types mirror GuestPayload and GuestResult in hostd/src/hypervisor.rs
// (serde JSON). testdata/payload.json and testdata/result.json are golden
// files that both sides test against, so a field renamed on one side fails a
// test on the other.
package guest

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// Port is the vsock port the agent listens on (GUEST_AGENT_PORT in hostd).
const Port = 1024

// ReadyPort accepts connections once the agent is listening on Port; hostd
// probes it before taking the base snapshot (GUEST_READY_PORT in hostd).
const ReadyPort = 1025

// MaxPayload bounds the payload frame. Files (mirror configs) are small; this
// only stops a corrupt length prefix from allocating gigabytes.
const MaxPayload = 64 << 20

// Payload is what hostd sends before the runner starts.
type Payload struct {
	JobID string `json:"job_id"`
	// RunnerConfigB64 is the single-use runner registration, base64.
	// Secret: never logged, never written to disk. String() omits it.
	RunnerConfigB64 string            `json:"runner_config_b64"`
	Env             map[string]string `json:"env"`
	// Files maps a path ("/etc/..." or "~/...") to base64 contents.
	Files    map[string]string `json:"files"`
	Disks    []Disk            `json:"disks"`
	Prebuild *Prebuild         `json:"prebuild"`
}

// Disk is a hot-plugged block device, found by its virtio serial.
type Disk struct {
	Serial string `json:"serial"`
	// Mount is "/" for the overlay upper layer, else a mount point.
	Mount string `json:"mount"`
}

// Prebuild is set when the run builds a prebuild: run the warm steps, scrub
// credentials, scan for secrets, and never start a runner.
type Prebuild struct {
	WarmCommands []string `json:"warm_commands"`
	KeepPaths    []string `json:"keep_paths"`
	ScrubPaths   []string `json:"scrub_paths"`
	// CloneCommands fetch and check out the repo in the workspace before the
	// warm steps. The clone token is in $TACHYON_CLONE_TOKEN for these
	// commands only, never for the warm steps.
	CloneCommands []string `json:"clone_commands,omitempty"`
	// CloneTokenB64 is the short-lived read-only clone token, base64. Secret.
	CloneTokenB64 string `json:"clone_token_b64,omitempty"`
	// Refresh: the disk is a fork of the previous prebuild; the clone
	// commands fetch only new objects.
	Refresh bool `json:"refresh,omitempty"`
}

// Result is the agent's final report.
type Result struct {
	ExitCode int `json:"exit_code"`
	// Phases maps a phase name (runner_online, job_started) to an RFC 3339
	// timestamp.
	Phases map[string]string `json:"phases"`
	// SecretScanClean is set for prebuild builds only.
	SecretScanClean *bool `json:"secret_scan_clean,omitempty"`
	// Failure names a failed prebuild step and ends with its last output, so
	// the failure is visible without the guest console.
	Failure string `json:"failure,omitempty"`
	// perf-only: the agent's steps, relative to the payload's arrival.
	Timeline []TimelineSpan `json:"timeline,omitempty"`
	// perf-only: the guest's wall clock at the payload's arrival, Unix
	// milliseconds, so hostd can measure the guest's clock skew.
	ReceivedUnixMS float64 `json:"received_unix_ms,omitempty"`
}

// Phase names reported in Result.Phases.
const (
	PhaseRunnerOnline = "runner_online"
	PhaseJobStarted   = "job_started"
)

// String omits the runner config, so a payload is safe to log.
func (p Payload) String() string {
	return fmt.Sprintf("Payload{job_id: %s, disks: %d, prebuild: %t}", p.JobID, len(p.Disks), p.Prebuild != nil)
}

// GoString keeps %#v from printing the runner config.
func (p Payload) GoString() string { return p.String() }

// RunnerConfig decodes the runner registration.
func (p *Payload) RunnerConfig() ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(p.RunnerConfigB64)
	if err != nil {
		return nil, fmt.Errorf("runner config is not base64")
	}
	return b, nil
}

// ReadFrame reads one 4-byte big-endian length-prefixed frame.
func ReadFrame(r io.Reader, max int) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, fmt.Errorf("read frame length: %w", err)
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if uint64(n) > uint64(max) {
		return nil, fmt.Errorf("frame too large: %d bytes", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, fmt.Errorf("read frame body: %w", err)
	}
	return body, nil
}

// WriteFrame writes one length-prefixed frame.
func WriteFrame(w io.Writer, body []byte) error {
	if uint64(len(body)) > 1<<32-1 {
		return fmt.Errorf("frame too large: %d bytes", len(body))
	}
	buf := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(buf, uint32(len(body)))
	copy(buf[4:], body)
	_, err := w.Write(buf)
	return err
}

// ReadPayload reads and decodes the payload frame.
func ReadPayload(r io.Reader) (*Payload, error) {
	body, err := ReadFrame(r, MaxPayload)
	if err != nil {
		return nil, err
	}
	var p Payload
	if err := json.Unmarshal(body, &p); err != nil {
		// Don't wrap: the decoder error can quote payload bytes.
		return nil, fmt.Errorf("payload is not valid JSON")
	}
	return &p, nil
}

// WriteResult encodes and writes the result frame.
func WriteResult(w io.Writer, r Result) error {
	if r.Phases == nil {
		r.Phases = map[string]string{}
	}
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return WriteFrame(w, body)
}
