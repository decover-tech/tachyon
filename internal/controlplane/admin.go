package controlplane

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	hostlinkv1 "github.com/decover-tech/tachyon/gen/hostlink/v1"
)

// ErrUnknownHost means no host with that ID has connected since the last
// restart and none was restored from the store.
var ErrUnknownHost = errors.New("unknown host")

type hostMeta struct {
	version string
	group   string
}

// HostView is the operator's view of one host.
type HostView struct {
	ID            string `json:"id"`
	Region        string `json:"region"`
	FailureDomain string `json:"failure_domain"`
	Arch          string `json:"arch"`
	// Group and HostdVersion are as of the host's last Hello; empty for a
	// restored host that has not reconnected.
	Group        string `json:"group"`
	HostdVersion string `json:"hostd_version"`
	VCPU         int    `json:"vcpu"`
	MemoryGB     int    `json:"memory_gb"`
	Connected    bool   `json:"connected"`
	// State is "active", "draining" (no new jobs, some still running) or
	// "drained" (no new jobs and none running: safe to take down).
	State          string     `json:"state"`
	DrainRequested bool       `json:"drain_requested"`
	Jobs           int        `json:"jobs"`
	Empty          bool       `json:"empty"`
	CPUReserved    float64    `json:"cpu_reserved"`
	MemReservedGB  float64    `json:"mem_reserved_gb"`
	LastHeartbeat  *time.Time `json:"last_heartbeat,omitempty"`
}

// SetDrain asks a host to stop taking jobs (or to take them again). The
// scheduler skips the host at once; hostd is told over its stream, and a
// host that is not connected is told when it next connects, so a drain
// survives the host restarting for an upgrade. Running jobs are not
// interrupted: poll HostViews until the host is Empty.
func (c *Core) SetDrain(id string, drain bool) error {
	c.mu.Lock()
	c.init()
	hc := c.hosts[id]
	if !c.Scheduler.SetDraining(id, drain) && hc == nil {
		c.mu.Unlock()
		return fmt.Errorf("%w: %q", ErrUnknownHost, id)
	}
	if drain {
		c.drainWanted[id] = true
	} else {
		delete(c.drainWanted, id)
	}
	if hc != nil {
		hc.draining = drain
	}
	c.mu.Unlock()
	if hc != nil {
		hc.send(&hostlinkv1.LinkResponse{Msg: &hostlinkv1.LinkResponse_Drain{Drain: &hostlinkv1.Drain{Drain: drain}}})
	}
	c.log().Info("host drain set by operator", "host", id, "drain", drain)
	return nil
}

// HostViews lists every known host, sorted by ID.
func (c *Core) HostViews() []HostView {
	status := c.Scheduler.Hosts()
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]HostView, 0, len(status))
	for _, st := range status {
		v := HostView{
			ID:             st.ID,
			Region:         st.Region,
			FailureDomain:  st.FailureDomain,
			Arch:           string(st.Arch),
			VCPU:           st.VCPU,
			MemoryGB:       st.MemGB,
			DrainRequested: c.drainWanted[st.ID],
			Jobs:           st.Jobs,
			CPUReserved:    st.CPUReserved,
			MemReservedGB:  st.MemReserved,
		}
		if m, ok := c.meta[st.ID]; ok {
			v.HostdVersion, v.Group = m.version, m.group
		}
		if hc := c.hosts[st.ID]; hc != nil {
			v.Connected = true
			if hb := hc.hb; hb != nil && hb.GetAt() != nil {
				t := hb.GetAt().AsTime()
				v.LastHeartbeat = &t
			}
		}
		draining := st.Draining || v.DrainRequested
		v.Empty = draining && st.Jobs == 0
		switch {
		case !draining:
			v.State = "active"
		case st.Jobs == 0:
			v.State = "drained"
		default:
			v.State = "draining"
		}
		out = append(out, v)
	}
	return out
}

// MinAdminTokenLen is the shortest admin token accepted.
const MinAdminTokenLen = 32

// LoadAdminToken reads the admin bearer token from path (surrounding
// whitespace trimmed). The token is never logged or put in an error.
func LoadAdminToken(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	tok := []byte(strings.TrimSpace(string(b)))
	if len(tok) < MinAdminTokenLen {
		return nil, fmt.Errorf("admin token in %s is shorter than %d bytes", path, MinAdminTokenLen)
	}
	return tok, nil
}

// AdminHandler serves the operator API:
//
//	GET  /admin/hosts[?group=G][&version=V]   hosts with state, version, jobs
//	POST /admin/hosts/{id}/drain              stop placing jobs on the host
//	POST /admin/hosts/{id}/undrain            place jobs on it again
//
// Every request, whatever the path, needs "Authorization: Bearer <token>".
// Serve it on its own listener (-admin), never on the webhook port.
func AdminHandler(c *Core, token []byte) (http.Handler, error) {
	if len(token) < MinAdminTokenLen {
		return nil, fmt.Errorf("admin token must be at least %d bytes", MinAdminTokenLen)
	}
	want := sha256.Sum256(token)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/hosts", func(w http.ResponseWriter, r *http.Request) {
		group, version := r.URL.Query().Get("group"), r.URL.Query().Get("version")
		hosts := []HostView{}
		versions := map[string]int{}
		for _, h := range c.HostViews() {
			if (group != "" && h.Group != group) || (version != "" && h.HostdVersion != version) {
				continue
			}
			hosts = append(hosts, h)
			versions[h.HostdVersion]++
		}
		writeJSON(w, http.StatusOK, map[string]any{"hosts": hosts, "versions": versions})
	})
	setDrain := func(drain bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("id")
			if err := c.SetDrain(id, drain); err != nil {
				code := http.StatusInternalServerError
				if errors.Is(err, ErrUnknownHost) {
					code = http.StatusNotFound
				}
				writeJSON(w, code, map[string]string{"error": err.Error()})
				return
			}
			for _, h := range c.HostViews() {
				if h.ID == id {
					writeJSON(w, http.StatusOK, h)
					return
				}
			}
			writeJSON(w, http.StatusOK, map[string]string{"id": id})
		}
	}
	mux.HandleFunc("POST /admin/hosts/{id}/drain", setDrain(true))
	mux.HandleFunc("POST /admin/hosts/{id}/undrain", setDrain(false))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		h := r.Header.Get("Authorization")
		// Hashing first makes the comparison constant-time in the
		// token's length too.
		got := sha256.Sum256([]byte(strings.TrimPrefix(h, prefix)))
		if !strings.HasPrefix(h, prefix) || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="tachyon-admin"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		mux.ServeHTTP(w, r)
	}), nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
