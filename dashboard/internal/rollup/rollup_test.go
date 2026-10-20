package rollup

import (
	"context"
	"encoding/json"
	"slices"
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
	r := New(st, time.Minute, 0, 90)
	r.spawn = func(f func()) { f() }
	return r, srv
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

	r.backfill(context.Background(), now, true)

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

func newRunnerAt(t *testing.T, names ...string) (*Runner, map[string]*valkeytest.Server) {
	t.Helper()
	cfg := &config.Config{DefaultRetentionDays: 90}
	servers := map[string]*valkeytest.Server{}
	for i, name := range names {
		srv := valkeytest.Start(t)
		servers[name] = srv
		cfg.Locations = append(cfg.Locations, config.Location{Name: name, Addr: srv.Addr(), Primary: i == 0})
	}
	st, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	servers[names[0]].SAdd(keys.Dashboards(), "platform")
	servers[names[0]].SAdd(keys.Services("platform"), "api")
	r := New(st, time.Minute, 0, 90)
	r.spawn = func(f func()) { f() }
	return r, servers
}

func storedAggregate(t *testing.T, srv *valkeytest.Server, hour time.Time) (store.AggregateRollup, bool) {
	t.Helper()
	raw, ok := srv.HGetAll(keys.RollupAggregateHour("platform", "api"))[strconv.FormatInt(hour.Unix(), 10)]
	if !ok {
		return store.AggregateRollup{}, false
	}
	var agg store.AggregateRollup
	if err := json.Unmarshal([]byte(raw), &agg); err != nil {
		t.Fatal(err)
	}
	return agg, true
}

func countBackfills(r *Runner) *int {
	n := new(int)
	r.spawn = func(f func()) { *n++; f() }
	return n
}

func TestAnHourStoredWithoutALocationIsCompletedOnceItAnswers(t *testing.T) {
	r, srv := newRunnerAt(t, "east", "west")
	backfills := countBackfills(r)
	today := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	hour := today.Add(-time.Hour)
	sample(srv["east"], hour.Add(10*time.Minute), true)
	sample(srv["west"], hour.Add(20*time.Minute), false)

	srv["west"].Fail("HGETALL", ":s:", "ERR west is down")
	r.runOnce(context.Background(), today.Add(30*time.Minute))
	agg, ok := storedAggregate(t, srv["east"], hour)
	if !ok || agg.Success != 1 || agg.Failure != 0 || len(agg.Missing) != 1 {
		t.Fatalf("23:00 while west is down = %+v, %v; want east's check stored and west missing", agg, ok)
	}
	r.runOnce(context.Background(), today.Add(40*time.Minute))
	if *backfills != 0 {
		t.Fatalf("%d backfills while west stayed down; want none", *backfills)
	}

	srv["west"].Heal()
	r.runOnce(context.Background(), today.Add(90*time.Minute))
	if *backfills != 1 {
		t.Fatalf("%d backfills once west answered; want 1", *backfills)
	}
	if agg, _ := storedAggregate(t, srv["east"], hour); agg.Success != 1 || agg.Failure != 1 || len(agg.Missing) != 0 {
		t.Fatalf("23:00 after west answered = %+v; want both locations' checks", agg)
	}
	if dr, ok := daily(t, srv["east"], today.AddDate(0, 0, -1)); !ok || dr.Success != 1 || dr.Failure != 1 {
		t.Fatalf("yesterday's daily rollup = %+v, %v; want it rebuilt with west's failure", dr, ok)
	}
	r.runOnce(context.Background(), today.Add(100*time.Minute))
	if *backfills != 1 {
		t.Fatalf("%d backfills after the recovery was handled; want 1", *backfills)
	}
}

func TestARecoveryDuringABackfillStartsAnotherOnceItEnds(t *testing.T) {
	r, srv := newRunnerAt(t, "east", "west")
	backfills := countBackfills(r)
	now := time.Date(2026, 10, 20, 10, 30, 0, 0, time.UTC)
	sample(srv["east"], now.Add(-time.Hour), true)
	srv["west"].Fail("HGETALL", ":s:", "ERR west is down")
	r.runOnce(context.Background(), now)
	srv["west"].Heal()
	r.backfilling = true
	r.runOnce(context.Background(), now.Add(5*time.Minute))
	if *backfills != 0 {
		t.Fatalf("%d backfills started while one was running; want none", *backfills)
	}
	r.backfilling = false
	r.runOnce(context.Background(), now.Add(10*time.Minute))
	if *backfills != 1 {
		t.Fatalf("%d backfills; want the recovery seen during a backfill to start one after it", *backfills)
	}
}

func TestAFailedWriteStartsABackfillOnTheNextCleanPass(t *testing.T) {
	r, srv := newRunner(t)
	backfills := countBackfills(r)
	now := time.Date(2026, 10, 20, 10, 30, 0, 0, time.UTC)
	sample(srv, now.Add(-time.Hour), true)
	srv.Fail("HSET", ":rollup:agg:hour", "ERR primary is restarting")
	r.runOnce(context.Background(), now)
	if *backfills != 0 {
		t.Fatalf("%d backfills from a pass whose writes failed; want none", *backfills)
	}
	srv.Heal()
	r.runOnce(context.Background(), now.Add(5*time.Minute))
	if *backfills != 1 {
		t.Fatalf("%d backfills on the first clean pass; want 1", *backfills)
	}
	r.runOnce(context.Background(), now.Add(10*time.Minute))
	if *backfills != 1 {
		t.Fatalf("%d backfills after the failure was handled; want 1", *backfills)
	}
}

func TestAFailedDailyWriteStartsABackfillOnTheNextCleanPass(t *testing.T) {
	r, srv := newRunner(t)
	backfills := countBackfills(r)
	today := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	sample(srv, today.Add(-30*time.Minute), true)
	srv.Fail("HSET", ":rollup:daily", "ERR primary is restarting")
	r.runOnce(context.Background(), today.Add(10*time.Minute))
	if *backfills != 0 {
		t.Fatalf("%d backfills from a pass whose daily write failed; want none", *backfills)
	}
	srv.Heal()
	r.runOnce(context.Background(), today.Add(15*time.Minute))
	if *backfills != 1 || !r.lastFull.Equal(today.Add(15*time.Minute)) {
		t.Fatalf("%d backfills, last full at %s, on the first clean pass after a failed daily write; want 1 full", *backfills, r.lastFull)
	}
}

func TestAMissSeenOnlyByABackfillIsRedoneOnTheNextCleanPass(t *testing.T) {
	r, srv := newRunnerAt(t, "east", "west")
	backfills := countBackfills(r)
	now := time.Date(2026, 10, 20, 10, 30, 0, 0, time.UTC)
	hour := now.Truncate(time.Hour).Add(-5 * time.Hour)
	sample(srv["east"], hour.Add(time.Minute), true)
	sample(srv["west"], hour.Add(2*time.Minute), false)
	srv["west"].Fail("HGETALL", ":s:", "ERR west is restarting")
	r.backfill(context.Background(), now, true)
	srv["west"].Heal()

	r.runOnce(context.Background(), now.Add(5*time.Minute))
	if *backfills != 1 {
		t.Fatalf("%d backfills after a miss seen only by a backfill; want 1", *backfills)
	}
	if agg, _ := storedAggregate(t, srv["east"], hour); agg.Failure != 1 || len(agg.Missing) != 0 {
		t.Fatalf("05:00 = %+v; want west's failure once west answered", agg)
	}
}

func TestAnHourLeftInProgressIsRedoneByTheNextBackfill(t *testing.T) {
	r, srv := newRunner(t)
	backfills := countBackfills(r)
	hour := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	sample(srv, hour.Add(10*time.Minute), true)
	r.runOnce(context.Background(), hour.Add(30*time.Minute))
	if agg, _ := storedAggregate(t, srv, hour); !agg.InProgress {
		t.Fatalf("10:00 written at 10:30 = %+v; want it marked in progress", agg)
	}

	sample(srv, hour.Add(40*time.Minute), false)
	srv.Fail("HSET", ":rollup:agg:hour", "ERR primary is restarting")
	r.runOnce(context.Background(), hour.Add(50*time.Minute))
	r.runOnce(context.Background(), hour.Add(90*time.Minute))
	srv.Heal()
	r.runOnce(context.Background(), hour.Add(150*time.Minute))
	if *backfills != 1 {
		t.Fatalf("%d backfills; want 1 once writes succeed again", *backfills)
	}
	if agg, _ := storedAggregate(t, srv, hour); agg.Success != 1 || agg.Failure != 1 || agg.InProgress {
		t.Fatalf("10:00 after the backfill = %+v; want the 10:40 failure and the hour finished", agg)
	}
}

func TestEachOutageIsBackfilledOnce(t *testing.T) {
	r, srv := newRunnerAt(t, "east", "west")
	backfills := countBackfills(r)
	now := time.Date(2026, 10, 20, 10, 30, 0, 0, time.UTC)
	sample(srv["east"], now.Add(-time.Hour), true)
	var got []int
	for i, westDown := range []bool{true, true, false, false, true, true, false, false} {
		if westDown {
			srv["west"].Fail("HGETALL", ":s:", "ERR west is down")
		} else {
			srv["west"].Heal()
		}
		r.runOnce(context.Background(), now.Add(time.Duration(i)*5*time.Minute))
		got = append(got, *backfills)
	}
	if want := []int{0, 0, 1, 1, 1, 1, 2, 2}; !slices.Equal(got, want) {
		t.Fatalf("backfills after each pass = %v; want %v, one per recovery", got, want)
	}
}

func TestBackfillRedoesOnlyHoursStoredWithoutALocation(t *testing.T) {
	r, srv := newRunnerAt(t, "east", "west")
	now := time.Date(2026, 10, 20, 10, 30, 0, 0, time.UTC)
	partial, complete := now.Truncate(time.Hour).Add(-5*time.Hour), now.Truncate(time.Hour).Add(-6*time.Hour)
	srv["east"].HSet(keys.RollupAggregateHour("platform", "api"), strconv.FormatInt(partial.Unix(), 10),
		`{"status":"operational","success":1,"perLocation":{"east":{"total":1,"success":1,"status":"success"}},"missing":["west"]}`)
	sample(srv["west"], partial.Add(time.Minute), false)
	storedHour(srv["east"], complete, 5, 0)
	sample(srv["west"], complete.Add(time.Minute), false)

	r.backfill(context.Background(), now, true)

	if agg, _ := storedAggregate(t, srv["east"], partial); agg.Success != 1 || agg.Failure != 1 || len(agg.Missing) != 0 {
		t.Fatalf("the partial hour after backfill = %+v; want east's stored check and west's failure", agg)
	}
	if agg, _ := storedAggregate(t, srv["east"], complete); agg.Success != 5 || agg.Failure != 0 {
		t.Fatalf("the complete hour after backfill = %+v; want it left alone", agg)
	}
}

func TestBackfillExpiresRollupFieldsStoredWithoutTTLs(t *testing.T) {
	r, srv := newRunner(t)
	now := time.Date(2026, 10, 20, 10, 30, 0, 0, time.UTC)
	old := strconv.FormatInt(now.Add(-100*24*time.Hour).Truncate(time.Hour).Unix(), 10)
	srv.HSet(keys.RollupAggregateHour("platform", "api"), old, `{"status":"operational","success":1}`)

	r.backfill(context.Background(), now, true)

	if _, ok := srv.HGetAll(keys.RollupAggregateHour("platform", "api"))[old]; ok {
		t.Fatal("backfill kept an hourly rollup 100 days old")
	}
}

func TestAFailedListingStartsABackfillOnTheNextCleanPass(t *testing.T) {
	r, srv := newRunner(t)
	backfills := countBackfills(r)
	now := time.Date(2026, 10, 20, 10, 30, 0, 0, time.UTC)
	sample(srv, now.Add(-time.Hour), true)
	for i, part := range []string{keys.Dashboards(), keys.Services("platform")} {
		srv.Fail("SMEMBERS", part, "ERR primary is restarting")
		r.runOnce(context.Background(), now)
		srv.Heal()
		r.runOnce(context.Background(), now.Add(5*time.Minute))
		if *backfills != i+1 {
			t.Fatalf("%d backfills after failing to list %s; want %d", *backfills, part, i+1)
		}
		if i == 0 && !r.lastFull.Equal(now.Add(5*time.Minute)) {
			t.Fatalf("last full backfill at %s; want a full one after failing to list %s", r.lastFull, part)
		}
	}
}

func TestAFollowUpBackfillRedoesOnlyMarkedHours(t *testing.T) {
	r, srv := newRunner(t)
	now := time.Date(2026, 10, 20, 10, 30, 0, 0, time.UTC)
	unstored, marked := now.Truncate(time.Hour).Add(-5*time.Hour), now.Truncate(time.Hour).Add(-6*time.Hour)
	sample(srv, unstored.Add(time.Minute), true)
	sample(srv, marked.Add(time.Minute), true)
	srv.HSet(keys.RollupAggregateHour("platform", "api"), strconv.FormatInt(marked.Unix(), 10),
		`{"status":"unknown","perLocation":{},"missing":["west"]}`)

	r.backfill(context.Background(), now, false)
	if _, ok := storedAggregate(t, srv, unstored); ok {
		t.Fatal("a follow-up backfill rolled up an hour that was never marked")
	}
	if agg, _ := storedAggregate(t, srv, marked); agg.Success != 1 || len(agg.Missing) != 0 {
		t.Fatalf("the marked hour after a follow-up backfill = %+v; want it redone", agg)
	}
	r.backfill(context.Background(), now, true)
	if _, ok := storedAggregate(t, srv, unstored); !ok {
		t.Fatal("a full backfill left an hour with no rollup")
	}
}

func TestStoreFailuresStartAFullBackfillAtMostHourly(t *testing.T) {
	r, srv := newRunner(t)
	backfills := countBackfills(r)
	now := time.Date(2026, 10, 20, 10, 30, 0, 0, time.UTC)
	r.lastFull = now.Add(-30 * time.Minute)
	sample(srv, now.Add(-time.Hour), true)
	srv.Fail("HSET", ":rollup:agg:hour", "ERR primary is restarting")
	r.runOnce(context.Background(), now)
	srv.Heal()
	r.runOnce(context.Background(), now.Add(5*time.Minute))
	if *backfills != 1 || !r.lastFull.Equal(now.Add(-30*time.Minute)) {
		t.Fatalf("%d backfills, last full at %s; want one follow-up backfill and no full one within the hour", *backfills, r.lastFull)
	}
	r.runOnce(context.Background(), now.Add(10*time.Minute))
	if *backfills != 1 {
		t.Fatalf("%d backfills on a quiet pass before the hour is up; want 1", *backfills)
	}
	r.runOnce(context.Background(), now.Add(35*time.Minute))
	if *backfills != 2 || !r.lastFull.Equal(now.Add(35*time.Minute)) {
		t.Fatalf("%d backfills, last full at %s; want the full backfill once the hour is up", *backfills, r.lastFull)
	}
	r.runOnce(context.Background(), now.Add(100*time.Minute))
	if *backfills != 2 {
		t.Fatalf("%d backfills an hour after the full one with nothing failing since; want 2", *backfills)
	}
}

func TestABackfillMarksOnlyHoursFromTheServicesFirstRollup(t *testing.T) {
	r, srv := newRunnerAt(t, "east", "west")
	now := time.Date(2026, 10, 20, 10, 30, 0, 0, time.UTC)
	first := now.Truncate(time.Hour).Add(-10 * time.Hour)
	storedHour(srv["east"], first, 1, 0)
	srv["west"].Fail("HGETALL", ":s:", "ERR west is down")
	r.backfill(context.Background(), now, true)

	if _, ok := storedAggregate(t, srv["east"], first.Add(-time.Hour)); ok {
		t.Fatal("an empty hour before the service's first rollup was marked")
	}
	if agg, ok := storedAggregate(t, srv["east"], first.Add(time.Hour)); !ok || len(agg.Missing) != 1 {
		t.Fatalf("an empty hour after the service's first rollup = %+v, %v; want it marked with west missing", agg, ok)
	}
}

func TestAnHourThatStaysUnreadableIsRetriedCheaply(t *testing.T) {
	r, srv := newRunnerAt(t, "east", "west")
	backfills := countBackfills(r)
	now := time.Date(2026, 10, 20, 10, 30, 0, 0, time.UTC)
	bad := now.Truncate(time.Hour).Add(-20 * time.Hour)
	storedHour(srv["east"], bad.Add(-time.Hour), 1, 0)
	sample(srv["west"], bad.Add(time.Minute), false)
	srv["west"].Fail("HGETALL", ":s:"+strconv.FormatInt(bad.Unix(), 10), "WRONGTYPE Operation against a key holding the wrong kind of value")
	r.backfill(context.Background(), now, true)
	r.lastFull = now

	for i := 1; i <= 3; i++ {
		before, primary := len(srv["west"].Commands()), len(srv["east"].Commands())
		r.runOnce(context.Background(), now.Add(time.Duration(i)*5*time.Minute))
		if sent := len(srv["west"].Commands()) - before; sent > 20 {
			t.Fatalf("pass %d sent %d commands to west; want a follow-up that redoes only the unreadable hour", i, sent)
		}
		for _, cmd := range srv["east"].Commands()[primary:] {
			if cmd[0] == "HKEYS" {
				t.Fatalf("pass %d reset rollup TTLs; only a full backfill does that", i)
			}
		}
		if sent := len(srv["east"].Commands()) - primary; sent > 40 {
			t.Fatalf("pass %d sent %d commands to the primary; want a follow-up that reads each service's rollups once", i, sent)
		}
	}
	if *backfills != 3 || !r.lastFull.Equal(now) {
		t.Fatalf("%d backfills, last full at %s; want one cheap follow-up per pass and no full one", *backfills, r.lastFull)
	}
}
