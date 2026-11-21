package dashboard

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/decover-tech/tachyon/internal/telemetry"
)

// Source supplies job records; *controlplane.Core implements it.
type Source interface {
	Records() []*telemetry.Record
}

// DefaultWindow is the report window when the request gives no since.
const DefaultWindow = 30 * 24 * time.Hour

// Handler serves a tenant's report as JSON:
//
//	GET ?since=RFC3339&until=RFC3339&repo=github.com/acme/api
//
// Tenant authenticates the request and names the caller's tenant. It is
// required: a Handler without one refuses every request, so a customer can
// never read another tenant's jobs.
type Handler struct {
	Source Source
	Rates  Rates
	Tenant func(*http.Request) (string, bool)
	Now    func() time.Time
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.Tenant == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	tenant, ok := h.Tenant(r)
	if !ok || tenant == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	now := time.Now()
	if h.Now != nil {
		now = h.Now()
	}
	q := r.URL.Query()
	opts := Options{Tenant: tenant, Repo: q.Get("repo"), Rates: h.Rates, Since: now.Add(-DefaultWindow)}
	for name, dst := range map[string]*time.Time{"since": &opts.Since, "until": &opts.Until} {
		if v := q.Get(name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				http.Error(w, name+": want RFC 3339 time", http.StatusBadRequest)
				return
			}
			*dst = t
		}
	}
	rep := Build(h.Source.Records(), opts)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rep)
}
