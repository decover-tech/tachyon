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
	"github.com/decover-tech/tachyon/internal/billing"
	"github.com/decover-tech/tachyon/internal/dashboard"
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
//	GET  /admin/billing/invoices[?period=YYYY-MM][&tenant=T]
//	                                          invoices priced by the meter
//	                                          (current month by default)
//	GET  /admin/dashboard?tenant=T[&since=][&until=][&repo=]
//	                                          a tenant's minutes-saved report
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
	mux.HandleFunc("GET /admin/billing/invoices", func(w http.ResponseWriter, r *http.Request) {
		b, ok := c.Meter.(Biller)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "billing is off (no prices configured)"})
			return
		}
		period := billing.PeriodOf(c.now())
		if v := r.URL.Query().Get("period"); v != "" {
			if _, err := time.Parse("2006-01", v); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "period: want YYYY-MM"})
				return
			}
			period = billing.Period(v)
		}
		tenants := b.Tenants(period)
		if t := r.URL.Query().Get("tenant"); t != "" {
			tenants = []string{t}
		}
		resp := InvoicesView{Period: string(period), Invoices: []InvoiceView{}}
		for _, t := range tenants {
			iv := invoiceView(b.Invoice(t, period))
			resp.Invoices = append(resp.Invoices, iv)
			resp.TotalMicroUSD += iv.TotalMicroUSD
		}
		resp.Total = billing.Money(resp.TotalMicroUSD).String()
		writeJSON(w, http.StatusOK, resp)
	})
	mux.Handle("GET /admin/dashboard", &dashboard.Handler{
		Source: c,
		Rates:  c.DashboardRates,
		Now:    c.now,
		Tenant: func(r *http.Request) (string, bool) {
			t := r.URL.Query().Get("tenant")
			return t, t != ""
		},
	})

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

// Biller prices metered usage into invoices; billing.Meter implements it.
type Biller interface {
	Tenants(p billing.Period) []string
	Invoice(tenant string, p billing.Period) billing.Invoice
}

// InvoicesView is the response of GET /admin/billing/invoices.
type InvoicesView struct {
	Period        string        `json:"period"`
	Invoices      []InvoiceView `json:"invoices"`
	TotalMicroUSD int64         `json:"total_micro_usd"`
	Total         string        `json:"total"`
}

// InvoiceView is one tenant's invoice. Amounts are micro-USD, with a
// formatted copy for people.
type InvoiceView struct {
	Tenant        string     `json:"tenant"`
	Period        string     `json:"period"`
	Lines         []LineView `json:"lines"`
	TotalMicroUSD int64      `json:"total_micro_usd"`
	Total         string     `json:"total"`
}

// LineView is one invoice line.
type LineView struct {
	Repo           string  `json:"repo"`
	Kind           string  `json:"kind"`
	VCPU           int     `json:"vcpu,omitempty"`
	Quantity       float64 `json:"quantity"`
	Unit           string  `json:"unit"`
	AmountMicroUSD int64   `json:"amount_micro_usd"`
	Amount         string  `json:"amount"`
}

func invoiceView(inv billing.Invoice) InvoiceView {
	v := InvoiceView{Tenant: inv.Tenant, Period: string(inv.Period), Lines: []LineView{},
		TotalMicroUSD: int64(inv.Total), Total: inv.Total.String()}
	for _, l := range inv.Lines {
		v.Lines = append(v.Lines, LineView{Repo: l.Repo, Kind: string(l.Kind), VCPU: l.VCPU, Quantity: l.Quantity,
			Unit: l.Unit, AmountMicroUSD: int64(l.Amount), Amount: l.Amount.String()})
	}
	return v
}
