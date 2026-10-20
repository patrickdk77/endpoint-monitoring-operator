package store

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/config"
	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/keys"
	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/valkeytest"
)

func newTestStore(t *testing.T, names ...string) (*Store, map[string]*valkeytest.Server) {
	t.Helper()
	cfg := &config.Config{DefaultRetentionDays: 90}
	servers := map[string]*valkeytest.Server{}
	for i, name := range names {
		srv := valkeytest.Start(t)
		servers[name] = srv
		cfg.Locations = append(cfg.Locations, config.Location{Name: name, Addr: srv.Addr(), Primary: i == 0})
	}
	st, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st, servers
}

func seedSample(srv *valkeytest.Server, dash, svc string, at time.Time, ok bool) {
	hour := at.UTC().Truncate(time.Hour).Unix()
	ts := strconv.FormatInt(at.Unix(), 10)
	st := "s"
	if !ok {
		st = "f"
	}
	srv.HSet(keys.StatusHour(dash, svc, hour), ts, st)
	srv.HSet(keys.DetailHour(dash, svc, hour), ts, "10")
}

func TestFakeValkeyServesTheRealClient(t *testing.T) {
	st, srv := newTestStore(t, "east")
	srv["east"].SAdd("emo:dashboards", "platform")
	got, err := st.ListDashboards(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "platform" {
		t.Fatalf("ListDashboards = %v, want [platform]", got)
	}
}

func TestListingsFailOnlyWhenEveryLocationFails(t *testing.T) {
	ctx := context.Background()
	st, srv := newTestStore(t, "east", "west")
	srv["east"].SAdd(keys.Dashboards(), "platform")
	srv["west"].SAdd(keys.Dashboards(), "edge")
	srv["east"].SAdd(keys.Services("platform"), "api")
	srv["west"].SAdd(keys.Services("platform"), "web")

	srv["west"].Fail("SMEMBERS", "emo:", "ERR west is down")
	dashboards, err := st.ListDashboards(ctx)
	if err != nil || len(dashboards) != 1 || dashboards[0] != "platform" {
		t.Fatalf("one location down: ListDashboards = %v, %v; want [platform], nil", dashboards, err)
	}
	services, err := st.ListServices(ctx, "platform")
	if err != nil || len(services) != 1 || services[0] != "api" {
		t.Fatalf("one location down: ListServices = %v, %v; want [api], nil", services, err)
	}

	srv["east"].Fail("SMEMBERS", "emo:", "ERR east is down")
	if dashboards, err := st.ListDashboards(ctx); err == nil {
		t.Fatalf("every location down: ListDashboards = %v, nil; want an error", dashboards)
	}
	if services, err := st.ListServices(ctx, "platform"); err == nil {
		t.Fatalf("every location down: ListServices = %v, nil; want an error", services)
	}
}

func TestGetMetaPrefersThePrimaryAndSkipsHashesWithoutAnEndpoint(t *testing.T) {
	ctx := context.Background()
	st, srv := newTestStore(t, "west", "east", "north")
	srv["west"].HSet(keys.Meta("platform", "api"), "sla7", "100%")
	srv["east"].HSet(keys.Meta("platform", "api"), "endpoint", "https://east.example/")
	srv["east"].HSet(keys.Meta("platform", "api"), "driver", "http")
	srv["north"].HSet(keys.Meta("platform", "api"), "endpoint", "https://north.example/")
	srv["north"].HSet(keys.Meta("platform", "api"), "driver", "http")
	for i := 0; i < 20; i++ {
		meta, err := st.GetMeta(ctx, "platform", "api")
		if err != nil || meta.Endpoint != "https://east.example/" {
			t.Fatalf("GetMeta = %+v, %v; want the east meta (first location by name with an endpoint)", meta, err)
		}
	}

	srv["west"].HSet(keys.Meta("platform", "api"), "endpoint", "https://west.example/")
	meta, err := st.GetMeta(ctx, "platform", "api")
	if err != nil || meta.Endpoint != "https://west.example/" {
		t.Fatalf("GetMeta = %+v, %v; want the primary's meta", meta, err)
	}
}

func TestGetMetaFallsBackWhenALocationFails(t *testing.T) {
	st, srv := newTestStore(t, "east", "west")
	srv["east"].HSet(keys.Meta("platform", "api"), "endpoint", "https://east.example/")
	srv["west"].HSet(keys.Meta("platform", "api"), "endpoint", "https://west.example/")
	srv["east"].Fail("HGETALL", ":meta", "ERR east is down")
	meta, err := st.GetMeta(context.Background(), "platform", "api")
	if err != nil || meta.Endpoint != "https://west.example/" {
		t.Fatalf("GetMeta = %+v, %v; want west's meta while the primary fails", meta, err)
	}
}

func TestHistoryStartsAtTheDayRetentionDaysAgo(t *testing.T) {
	st, srv := newTestStore(t, "east")
	now := time.Date(2026, 10, 20, 12, 30, 0, 0, time.UTC)
	first := now.AddDate(0, 0, -90).Truncate(24 * time.Hour)
	for _, at := range []time.Time{first.Add(-time.Hour), first, now.Add(-time.Hour)} {
		srv["east"].HSet(keys.RollupAggregateHour("platform", "api"), strconv.FormatInt(at.Unix(), 10), `{"status":"operational","success":1}`)
	}
	hist, err := st.History(context.Background(), "platform", "api", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := hist.Hours[first.Unix()]; !ok {
		t.Errorf("History left out %s, the start of the day 90 days back", first)
	}
	if _, ok := hist.Hours[now.Add(-time.Hour).Unix()]; !ok {
		t.Errorf("History left out the previous hour")
	}
	if _, ok := hist.Hours[first.Add(-time.Hour).Unix()]; ok {
		t.Errorf("History included %s, before its window", first.Add(-time.Hour))
	}
}

func TestHistoryFailsInsteadOfPanickingWhenTheRollupReadFails(t *testing.T) {
	st, srv := newTestStore(t, "east")
	seedSample(srv["east"], "platform", "api", time.Now(), true)
	srv["east"].Fail("HGETALL", ":rollup:agg:hour", "ERR primary is restarting")
	if _, err := st.History(context.Background(), "platform", "api", time.Now().UTC()); err == nil {
		t.Fatal("History succeeded although the rollup read failed")
	}
}

func TestFailureWindowsFailsInsteadOfPanickingWhenTheRollupReadFails(t *testing.T) {
	st, srv := newTestStore(t, "east")
	now := time.Now().UTC()
	seedSample(srv["east"], "platform", "api", now, false)
	srv["east"].Fail("HGETALL", ":rollup:agg:hour", "ERR primary is restarting")
	if _, err := st.FailureWindows(context.Background(), "platform", "api", now.Add(-time.Hour), now, now, true); err == nil {
		t.Fatal("FailureWindows succeeded although the rollup read failed")
	}
}

func TestStoredRollupsTakeTheirStatusFromTheirLocations(t *testing.T) {
	st, srv := newTestStore(t, "east")
	hour := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	srv["east"].HSet(keys.RollupAggregateHour("platform", "api"), strconv.FormatInt(hour.Unix(), 10),
		`{"status":"major_outage","success":119,"failure":1,"perLocation":{"east":{"total":120,"success":119,"failure":1,"status":"degraded"}}}`)
	srv["east"].HSet(keys.RollupAggregateHour("platform", "api"), strconv.FormatInt(hour.Add(time.Hour).Unix(), 10),
		`{"status":"degraded","success":1,"failure":1}`)
	rollups, err := st.GetAggregateRollups(context.Background(), "platform", "api", hour, hour.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got := rollups[hour.Unix()].Status; got != "degraded" {
		t.Fatalf("a stored hour with one failed check in 120 at its only location reads as %q, want degraded", got)
	}
	if got := rollups[hour.Add(time.Hour).Unix()].Status; got != "degraded" {
		t.Fatalf("a stored hour without per-location counts reads as %q, want its stored status", got)
	}
}
