package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/decover-tech/tachyon/internal/billing"
	"github.com/decover-tech/tachyon/internal/store"
	"github.com/decover-tech/tachyon/internal/telemetry"
)

func finishedRecord(id, tenant string, vcpu int, start time.Time, d time.Duration) *telemetry.Record {
	r := telemetry.NewRecord(id)
	r.Tenant, r.Repo, r.VCPU = tenant, "github.com/"+tenant+"/api", vcpu
	r.Mark(telemetry.VMResumed, start)
	r.Mark(telemetry.JobFinished, start.Add(d))
	return r
}

func testMeter() *billing.Meter {
	return billing.NewMeter(billing.PriceBook{RunnerPerMinute: map[int]billing.Money{4: 8000}})
}

func TestAdminBillingOff(t *testing.T) {
	e := newEnv()
	if rec := adminDo(t, e.admin(t), "GET", "/admin/billing/invoices", testAdminToken); rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestAdminBillingInvoices(t *testing.T) {
	e := newEnv()
	m := testMeter()
	e.core.Meter = m
	at := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	e.core.Now = func() time.Time { return at }
	for _, r := range []*telemetry.Record{
		finishedRecord("j1", "acme", 4, at, 90*time.Second),
		finishedRecord("j2", "acme", 4, at, 30*time.Second),
		finishedRecord("j3", "zeta", 4, at, 60*time.Second),
	} {
		if err := m.RecordJob(r); err != nil {
			t.Fatal(err)
		}
	}
	h := e.admin(t)
	if rec := adminDo(t, h, "GET", "/admin/billing/invoices", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: code = %d", rec.Code)
	}
	if rec := adminDo(t, h, "GET", "/admin/billing/invoices?period=nov", testAdminToken); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad period: code = %d", rec.Code)
	}

	var all InvoicesView
	rec := adminDo(t, h, "GET", "/admin/billing/invoices", testAdminToken)
	if err := json.Unmarshal(rec.Body.Bytes(), &all); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("code %d body %s err %v", rec.Code, rec.Body, err)
	}
	// 2 min of acme + 1 min of zeta at $0.008/min.
	if all.Period != "2026-11" || len(all.Invoices) != 2 || all.TotalMicroUSD != 24000 || all.Total != "$0.024000" {
		t.Fatalf("invoices = %+v", all)
	}
	if a := all.Invoices[0]; a.Tenant != "acme" || a.TotalMicroUSD != 16000 || len(a.Lines) != 1 || a.Lines[0].Quantity != 120 {
		t.Fatalf("acme = %+v", a)
	}

	var one InvoicesView
	rec = adminDo(t, h, "GET", "/admin/billing/invoices?tenant=zeta&period=2026-11", testAdminToken)
	if err := json.Unmarshal(rec.Body.Bytes(), &one); err != nil || len(one.Invoices) != 1 || one.TotalMicroUSD != 8000 {
		t.Fatalf("zeta = %+v %v", one, err)
	}
	rec = adminDo(t, h, "GET", "/admin/billing/invoices?period=2026-10", testAdminToken)
	if err := json.Unmarshal(rec.Body.Bytes(), &one); err != nil || len(one.Invoices) != 0 || one.TotalMicroUSD != 0 {
		t.Fatalf("october = %+v %v", one, err)
	}
}

func TestAdminDashboardNeedsTenant(t *testing.T) {
	e := newEnv()
	h := e.admin(t)
	if rec := adminDo(t, h, "GET", "/admin/dashboard", testAdminToken); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no tenant: code = %d", rec.Code)
	}
	if rec := adminDo(t, h, "GET", "/admin/dashboard?tenant=acme", testAdminToken); rec.Code != http.StatusOK {
		t.Fatalf("code = %d body %s", rec.Code, rec.Body)
	}
}

func TestReplayMeterRestoresUsage(t *testing.T) {
	ctx := context.Background()
	st := &store.Mem{}
	now := time.Now().UTC()
	for _, r := range []*telemetry.Record{
		finishedRecord("new", "acme", 4, now.Add(-time.Hour), time.Minute),
		finishedRecord("old", "acme", 4, now.AddDate(0, -3, 0), time.Minute),
	} {
		if err := st.PutRecord(ctx, r.Data()); err != nil {
			t.Fatal(err)
		}
	}
	unfinished := telemetry.NewRecord("running")
	unfinished.Tenant, unfinished.VCPU = "acme", 4
	if err := st.PutRecord(ctx, unfinished.Data()); err != nil {
		t.Fatal(err)
	}
	m := testMeter()
	c := &Core{Store: st, Meter: m}
	since := now.AddDate(0, -1, 0)
	for range 2 { // replaying twice must not double-bill
		n, err := c.ReplayMeter(ctx, since)
		if err != nil || n != 1 {
			t.Fatalf("ReplayMeter = %d %v", n, err)
		}
	}
	inv := m.Invoice("acme", billing.PeriodOf(now.Add(-time.Hour).Add(time.Minute)))
	if inv.Total != 8000 {
		t.Fatalf("invoice = %+v", inv)
	}
}
