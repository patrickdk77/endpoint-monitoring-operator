package rollup

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/config"
	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/keys"
	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/store"
	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/valkeytest"
)

func newRunner(t *testing.T) (*Runner, *valkeytest.Server) {
	t.Helper()
	srv := valkeytest.Start(t)
	st, err := store.New(&config.Config{
		DefaultRetentionDays: 90,
		Locations:            []config.Location{{Name: "east", Addr: srv.Addr(), Primary: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	srv.SAdd(keys.Dashboards(), "platform")
	srv.SAdd(keys.Services("platform"), "api")
	return New(st, time.Minute, 0, 90), srv
}

func sample(srv *valkeytest.Server, at time.Time, ok bool) {
	hour := at.Truncate(time.Hour).Unix()
	ts := strconv.FormatInt(at.Unix(), 10)
	st := "s"
	if !ok {
		st = "f"
	}
	srv.HSet(keys.StatusHour("platform", "api", hour), ts, st)
	srv.HSet(keys.DetailHour("platform", "api", hour), ts, "10")
}

func storedHour(srv *valkeytest.Server, hour time.Time, success, failure int) {
	srv.HSet(keys.RollupAggregateHour("platform", "api"), strconv.FormatInt(hour.Unix(), 10),
		`{"status":"operational","success":`+strconv.Itoa(success)+`,"failure":`+strconv.Itoa(failure)+`}`)
}

func daily(t *testing.T, srv *valkeytest.Server, day time.Time) (store.DailyRollup, bool) {
	t.Helper()
	raw, ok := srv.HGetAll(keys.RollupDaily("platform", "api"))[day.Format("2006-01-02")]
	if !ok {
		return store.DailyRollup{}, false
	}
	var dr store.DailyRollup
	if err := json.Unmarshal([]byte(raw), &dr); err != nil {
		t.Fatal(err)
	}
	return dr, true
}

func TestRunOnceRebuildsYesterdaysDailyRollupInTheFirstHour(t *testing.T) {
	r, srv := newRunner(t)
	today := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	yesterday := today.AddDate(0, 0, -1)
	storedHour(srv, yesterday.Add(10*time.Hour), 5, 0)
	sample(srv, yesterday.Add(23*time.Hour+10*time.Minute), true)
	sample(srv, yesterday.Add(23*time.Hour+20*time.Minute), false)

	r.runOnce(context.Background(), today.Add(30*time.Minute))

	dr, ok := daily(t, srv, yesterday)
	if !ok || dr.Success != 6 || dr.Failure != 1 {
		t.Fatalf("yesterday's daily rollup = %+v, %v; want 6 successes and 1 failure", dr, ok)
	}
}

func TestRunOnceRebuildsYesterdaysDailyRollupWhenItsLastHourIsEmpty(t *testing.T) {
	r, srv := newRunner(t)
	today := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	yesterday := today.AddDate(0, 0, -1)
	storedHour(srv, yesterday.Add(10*time.Hour), 5, 0)
	storedHour(srv, yesterday.Add(22*time.Hour), 1, 0)

	r.runOnce(context.Background(), today.Add(5*time.Minute))

	dr, ok := daily(t, srv, yesterday)
	if !ok || dr.Success != 6 || dr.Failure != 0 {
		t.Fatalf("yesterday's daily rollup = %+v, %v; want 6 successes although 23:00 had no checks", dr, ok)
	}
}

func TestRunOnceWritesNoDailyRollupLaterInTheDay(t *testing.T) {
	r, srv := newRunner(t)
	today := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	sample(srv, today.Add(9*time.Hour+10*time.Minute), true)

	r.runOnce(context.Background(), today.Add(10*time.Hour+30*time.Minute))

	if all := srv.HGetAll(keys.RollupDaily("platform", "api")); len(all) != 0 {
		t.Fatalf("daily rollups written for an unfinished day: %v", all)
	}
	if _, ok := srv.HGetAll(keys.RollupAggregateHour("platform", "api"))[strconv.FormatInt(today.Add(9*time.Hour).Unix(), 10)]; !ok {
		t.Fatal("the previous hour was not rolled up")
	}
}

func TestRunOnceNoLongerWritesSLAFieldsIntoMeta(t *testing.T) {
	r, srv := newRunner(t)
	now := time.Date(2026, 10, 20, 10, 30, 0, 0, time.UTC)
	srv.HSet(keys.Meta("platform", "api"), "endpoint", "https://api.example/")
	sample(srv, now.Add(-10*time.Minute), true)

	r.runOnce(context.Background(), now)

	for _, f := range []string{"sla7", "sla30", "sla365"} {
		if _, ok := srv.HGetAll(keys.Meta("platform", "api"))[f]; ok {
			t.Errorf("runOnce wrote %s into the meta hash", f)
		}
	}
}

func TestBackfillRebuildsMissingDailyRollups(t *testing.T) {
	r, srv := newRunner(t)
	now := time.Date(2026, 10, 20, 10, 30, 0, 0, time.UTC)
	today := now.Truncate(24 * time.Hour)
	// Day -1: no daily and a raw check backfill rolls up.
	sample(srv, today.AddDate(0, 0, -1).Add(5*time.Hour), false)
	// Day -2: no daily, only a stored hourly rollup.
	storedHour(srv, today.AddDate(0, 0, -2).Add(3*time.Hour), 8, 2)
	// Day -3: a short daily that misses a raw check backfill now rolls up.
	storedHour(srv, today.AddDate(0, 0, -3).Add(4*time.Hour), 4, 0)
	srv.HSet(keys.RollupDaily("platform", "api"), today.AddDate(0, 0, -3).Format("2006-01-02"), `{"success":4,"total":4}`)
	sample(srv, today.AddDate(0, 0, -3).Add(6*time.Hour), true)
	// Day -4: untouched, with a daily that differs from its hourly rollups.
	storedHour(srv, today.AddDate(0, 0, -4).Add(4*time.Hour), 1, 0)
	srv.HSet(keys.RollupDaily("platform", "api"), today.AddDate(0, 0, -4).Format("2006-01-02"), `{"success":50,"failure":5,"total":55}`)
	storedHour(srv, today.Add(time.Hour), 3, 0)

	r.backfill(context.Background(), now)

	for _, tc := range []struct {
		daysAgo          int
		success, failure int
	}{
		{1, 0, 1},
		{2, 8, 2},
		{3, 5, 0},
		{4, 50, 5},
	} {
		dr, ok := daily(t, srv, today.AddDate(0, 0, -tc.daysAgo))
		if !ok || dr.Success != tc.success || dr.Failure != tc.failure {
			t.Errorf("daily rollup %d days ago = %+v, %v; want %d/%d", tc.daysAgo, dr, ok, tc.success, tc.failure)
		}
	}
	if _, ok := daily(t, srv, today); ok {
		t.Error("backfill wrote a daily rollup for today, which has not finished")
	}
}
