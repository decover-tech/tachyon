package telemetry

import (
	"net/http"
	"strconv"
	"strings"
)

// perf-only: TimedTransport records every request made with a context that
// carries a Timeline as a span named "<name> <METHOD> <route>", where route
// is the URL path with ids replaced (/repos/{owner}/{repo}/actions/runs/{n}),
// so spans from different jobs group by endpoint. Requests without a
// timeline pass straight through.
func TimedTransport(name string, base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &timedTransport{name: name, base: base}
}

type timedTransport struct {
	name string
	base http.RoundTripper
}

func (t *timedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	if TimelineFrom(ctx) == nil {
		return t.base.RoundTrip(req)
	}
	end := StartSpan(ctx, t.name+" "+req.Method+" "+Route(req.URL.Path))
	defer end()
	resp, err := t.base.RoundTrip(req)
	switch {
	case err != nil:
		Annotate(ctx, "error", err.Error())
	default:
		Annotate(ctx, "status", strconv.Itoa(resp.StatusCode))
	}
	return resp, err
}

// Route replaces the variable parts of a REST path: numeric segments become
// {n}, and the owner and repo after /repos/ become {owner}/{repo}.
func Route(path string) string {
	seg := strings.Split(strings.Trim(path, "/"), "/")
	for i, s := range seg {
		switch {
		case i > 0 && seg[0] == "repos" && i == 1:
			seg[i] = "{owner}"
		case i > 0 && seg[0] == "repos" && i == 2:
			seg[i] = "{repo}"
		case s != "" && strings.Trim(s, "0123456789") == "":
			seg[i] = "{n}"
		}
	}
	return "/" + strings.Join(seg, "/")
}
