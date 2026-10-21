package store

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/keys"
	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/valkeytest"
)

func rawCheck(srv *valkeytest.Server, at time.Time, ok bool, ms int) {
	hour := at.Truncate(time.Hour).Unix()
	ts := strconv.FormatInt(at.Unix(), 10)
	st := "s"
	if !ok {
		st = "f"
	}
	srv.HSet(keys.StatusHour("platform", "api", hour), ts, st)
	srv.HSet(keys.DetailHour("platform", "api", hour), ts, strconv.Itoa(ms))
}

func TestLatencyReturnsEveryCheckUnderTheLimit(t *testing.T) {
	st, srv := newTestStore(t, "east")
	start := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		rawCheck(srv["east"], start.Add(time.Duration(i)*time.Minute), i != 3 && i != 7, 100+10*i)
	}
	lat, err := st.Latency(context.Background(), "platform", "api", start, start.Add(time.Hour), 1440)
	if err != nil {
		t.Fatal(err)
	}
	if lat.Bucket != 0 || len(lat.Series["east"]) != 10 {
		t.Fatalf("bucket %d, %d points; want every check as its own point", lat.Bucket, len(lat.Series["east"]))
	}
	failed := lat.Series["east"][3]
	if failed.Failed != 1 || failed.Avg != nil {
		t.Fatalf("a failed check plotted as %+v; want failed 1 and no latency", failed)
	}
	s := lat.Stats["east"]
	if s.Checks != 10 || s.Failed != 2 || *s.Min != 100 || *s.Max != 190 {
		t.Fatalf("stats %+v min %v max %v; want 10 checks, 2 failed, latency 100 to 190 from successes only", s, *s.Min, *s.Max)
	}
	if want := float64(100+110+120+140+150+160+180+190) / 8; *s.Avg != want {
		t.Fatalf("avg %v, want %v", *s.Avg, want)
	}
}

func TestLatencyGroupsChecksAboveTheLimitKeepingSpikes(t *testing.T) {
	st, srv := newTestStore(t, "east")
	start := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 30; i++ {
		ms := 100
		if i == 17 {
			ms = 999
		}
		rawCheck(srv["east"], start.Add(time.Duration(i)*time.Minute), true, ms)
	}
	lat, err := st.Latency(context.Background(), "platform", "api", start, start.Add(30*time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	pts := lat.Series["east"]
	if lat.Bucket != 181 || len(pts) > 10 {
		t.Fatalf("bucket %d with %d points; want 181s buckets and at most 10 points", lat.Bucket, len(pts))
	}
	checks, peak := 0, 0.0
	for _, p := range pts {
		checks += p.Checks
		if p.Max != nil && *p.Max > peak {
			peak = *p.Max
		}
	}
	if checks != 30 || peak != 999 {
		t.Fatalf("buckets hold %d checks with peak %v; want 30 checks and the 999 ms spike", checks, peak)
	}
}

func TestLatencyUsesTheRollupOnceAnHoursChecksHaveExpired(t *testing.T) {
	st, srv := newTestStore(t, "east")
	start := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	rollup := func(hour time.Time) {
		srv["east"].HSet(keys.RollupAggregateHour("platform", "api"), strconv.FormatInt(hour.Unix(), 10),
			`{"status":"degraded","success":58,"failure":2,"perLocation":{"east":{"total":60,"success":58,"failure":2,"avgMs":100,"minMs":50,"maxMs":300}}}`)
	}
	rollup(start)
	rollup(start.Add(time.Hour))
	rawCheck(srv["east"], start.Add(time.Hour+5*time.Minute), true, 70)
	lat, err := st.Latency(context.Background(), "platform", "api", start, start.Add(2*time.Hour), 1440)
	if err != nil {
		t.Fatal(err)
	}
	pts := lat.Series["east"]
	if len(pts) != 2 || pts[0].T != start.Unix() || pts[0].Checks != 60 || *pts[0].Max != 300 {
		t.Fatalf("points %+v; want the expired hour from its rollup, then the one raw check", pts)
	}
	if pts[0].Span != 3600 || pts[1].Span != 0 {
		t.Fatalf("spans %d and %d; want 3600 for the rollup hour and 0 for the check", pts[0].Span, pts[1].Span)
	}
	if s := lat.Stats["east"]; s.Checks != 61 || s.Failed != 2 || *s.Min != 50 || *s.Max != 300 {
		t.Fatalf("stats %+v; want 61 checks: the rollup hour plus the raw check, not the second rollup", s)
	}
}

func TestLatencyFailsWhenTheRollupReadFails(t *testing.T) {
	st, srv := newTestStore(t, "east")
	srv["east"].Fail("HGETALL", ":rollup:agg:hour", "ERR primary is restarting")
	now := time.Now().UTC()
	if _, err := st.Latency(context.Background(), "platform", "api", now.Add(-time.Hour), now, 1440); err == nil {
		t.Fatal("Latency succeeded although the rollup read failed")
	}
}

func TestRollupLatencyCountsSuccessesOnlyAndWeighsLocations(t *testing.T) {
	st, srv := newTestStore(t, "east", "west")
	hour := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	rawCheck(srv["east"], hour.Add(1*time.Minute), true, 100)
	rawCheck(srv["east"], hour.Add(2*time.Minute), true, 100)
	rawCheck(srv["east"], hour.Add(3*time.Minute), true, 100)
	rawCheck(srv["east"], hour.Add(4*time.Minute), false, 30000)
	rawCheck(srv["west"], hour.Add(1*time.Minute), true, 400)
	agg, _ := st.computeAggregate(context.Background(), "platform", "api", hour)
	if len(agg.PerLocation) == 0 {
		t.Fatal("no aggregate")
	}
	east := agg.PerLocation["east"]
	if east.AvgMs != 100 || east.MinMs != 100 || east.MaxMs != 100 {
		t.Fatalf("east rollup latency %v (min %v max %v); want 100, the timed-out failure left out", east.AvgMs, east.MinMs, east.MaxMs)
	}
	if agg.AvgMs != 175 {
		t.Fatalf("aggregate latency %v, want 175: three east successes at 100 and one west at 400", agg.AvgMs)
	}
}

func storedRollup(srv *valkeytest.Server, hour time.Time, perLocation string) {
	srv.HSet(keys.RollupAggregateHour("platform", "api"), strconv.FormatInt(hour.Unix(), 10),
		`{"status":"operational","perLocation":{`+perLocation+`}}`)
}

func TestLatencyLeavesOutWhatStartsBeforeTheRange(t *testing.T) {
	st, srv := newTestStore(t, "east")
	hour := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	for _, m := range []int{5, 15, 25, 35, 45} {
		rawCheck(srv["east"], hour.Add(time.Duration(m)*time.Minute), true, 100)
	}
	storedRollup(srv["east"], hour, `"east":{"total":60,"success":60,"avgMs":100,"minMs":100,"maxMs":100,"measured":60}`)
	lat, err := st.Latency(context.Background(), "platform", "api", hour.Add(30*time.Minute), hour.Add(50*time.Minute), 1440)
	if err != nil {
		t.Fatal(err)
	}
	if pts := lat.Series["east"]; len(pts) != 2 || lat.Stats["east"].Checks != 2 {
		t.Fatalf("points %+v, stats %+v; want only the 10:35 and 10:45 checks", pts, lat.Stats["east"])
	}
}

func TestLatencyUsesTheFullerRollupForAPartlyExpiredSettledHour(t *testing.T) {
	st, srv := newTestStore(t, "east")
	to := time.Date(2026, 10, 20, 12, 30, 0, 0, time.UTC)
	settled := to.Truncate(time.Hour).Add(-3 * time.Hour)
	previous := to.Truncate(time.Hour).Add(-time.Hour)
	for i := 0; i < 20; i++ {
		rawCheck(srv["east"], settled.Add(time.Duration(40+i)*time.Minute), true, 100)
	}
	for i := 0; i < 10; i++ {
		rawCheck(srv["east"], previous.Add(time.Duration(i)*time.Minute), true, 100)
	}
	storedRollup(srv["east"], settled, `"east":{"total":60,"success":60,"avgMs":100,"minMs":100,"maxMs":100,"measured":60}`)
	storedRollup(srv["east"], previous, `"east":{"total":60,"success":60,"avgMs":100,"minMs":100,"maxMs":100,"measured":60}`)
	lat, err := st.Latency(context.Background(), "platform", "api", settled, to, 1440)
	if err != nil {
		t.Fatal(err)
	}
	if got := lat.Stats["east"].Checks; got != 70 {
		t.Fatalf("checks %d; want 70: the settled hour's rollup (60) plus the previous hour's raw checks (10)", got)
	}
}

func TestLatencyServesALocationFromRollupsWhenItsRawReadFails(t *testing.T) {
	st, srv := newTestStore(t, "east", "west")
	hour := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	rawCheck(srv["east"], hour.Add(5*time.Minute), true, 100)
	storedRollup(srv["east"], hour, `"east":{"total":1,"success":1,"avgMs":100,"minMs":100,"maxMs":100,"measured":1},`+
		`"west":{"total":30,"success":30,"avgMs":250,"minMs":200,"maxMs":300,"measured":30}`)
	srv["west"].Fail("HGETALL", ":s:", "ERR west is down")
	lat, err := st.Latency(context.Background(), "platform", "api", hour, hour.Add(time.Hour), 1440)
	if err != nil {
		t.Fatal(err)
	}
	if pts := lat.Series["west"]; len(pts) != 1 || pts[0].Checks != 30 || *pts[0].Max != 300 {
		t.Fatalf("west points %+v; want its rollup hour while its raw read fails", pts)
	}
}

func TestLatencyBucketsEverySeriesAlike(t *testing.T) {
	st, srv := newTestStore(t, "east", "west")
	start := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 30; i++ {
		rawCheck(srv["east"], start.Add(time.Duration(i)*time.Minute), true, 100)
	}
	for i := 0; i < 3; i++ {
		rawCheck(srv["west"], start.Add(time.Duration(10*i+5)*time.Minute), true, 200)
	}
	lat, err := st.Latency(context.Background(), "platform", "api", start, start.Add(30*time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(lat.Series["west"]); n != 3 {
		t.Fatalf("west has %d points, want one per check: %+v", n, lat.Series["west"])
	}
	for _, p := range lat.Series["west"] {
		if (p.T-lat.From)%lat.Bucket != 0 || p.Span != lat.Bucket || p.Checks != 1 {
			t.Fatalf("west point %+v is not a bucket of %ds from %d holding one check", p, lat.Bucket, lat.From)
		}
	}
}

func TestLatencyPlotsEachCheckOfAFinishedHourThatHasARollup(t *testing.T) {
	st, srv := newTestStore(t, "east")
	to := time.Date(2026, 10, 20, 12, 30, 0, 0, time.UTC)
	hour := to.Truncate(time.Hour).Add(-3 * time.Hour)
	for i := 0; i < 60; i++ {
		rawCheck(srv["east"], hour.Add(time.Duration(i)*time.Minute), true, 100+i)
	}
	storedRollup(srv["east"], hour, `"east":{"total":60,"success":60,"avgMs":129.5,"minMs":100,"maxMs":159,"measured":60}`)
	lat, err := st.Latency(context.Background(), "platform", "api", hour, to, 0)
	if err != nil {
		t.Fatal(err)
	}
	pts := lat.Series["east"]
	if len(pts) != 60 || pts[0].Span != 0 || *pts[59].Avg != 159 {
		t.Fatalf("%d points, first %+v; want the hour's 60 checks, not its rollup", len(pts), pts[0])
	}
}

func TestLatencyReadsOldAndUnmeasuredRollups(t *testing.T) {
	st, srv := newTestStore(t, "east")
	hour := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	storedRollup(srv["east"], hour, `"east":{"total":10,"success":10,"avgMs":120}`)
	storedRollup(srv["east"], hour.Add(time.Hour), `"east":{"total":5,"success":5,"avgMs":0}`)
	lat, err := st.Latency(context.Background(), "platform", "api", hour, hour.Add(3*time.Hour), 1440)
	if err != nil {
		t.Fatal(err)
	}
	pts := lat.Series["east"]
	if len(pts) != 2 || pts[0].Avg == nil || *pts[0].Min != 120 || *pts[0].Max != 120 {
		t.Fatalf("points %+v; want the old rollup at 120 ms with min and max taken from its average", pts)
	}
	if pts[1].Avg != nil || pts[1].Checks != 5 {
		t.Fatalf("a rollup with no measured latency plotted %+v; want 5 checks and no latency", pts[1])
	}
}

func TestLatencyKeepsAChecksAtTheEndInsideTheCap(t *testing.T) {
	st, srv := newTestStore(t, "east")
	start := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	for i := 0; i <= 30; i++ {
		rawCheck(srv["east"], start.Add(time.Duration(i)*time.Minute), true, 100)
	}
	lat, err := st.Latency(context.Background(), "platform", "api", start, start.Add(30*time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(lat.Series["east"]); n > 10 {
		t.Fatalf("%d points with a check exactly at the end; want at most 10", n)
	}
}

func TestRollupMinAndMaxAreTheExtremes(t *testing.T) {
	st, srv := newTestStore(t, "east")
	hour := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	for i, ms := range []int{100, 80, 120} {
		rawCheck(srv["east"], hour.Add(time.Duration(i+1)*time.Minute), true, ms)
	}
	agg, _ := st.computeAggregate(context.Background(), "platform", "api", hour)
	if len(agg.PerLocation) == 0 {
		t.Fatal("no aggregate")
	}
	if east := agg.PerLocation["east"]; east.MinMs != 80 || east.MaxMs != 120 || east.Measured != 3 {
		t.Fatalf("east min %v max %v measured %d; want 80, 120 and 3", east.MinMs, east.MaxMs, east.Measured)
	}
}

func TestSuccessesWithoutLatencyDoNotPullTheAverageDown(t *testing.T) {
	st, srv := newTestStore(t, "east", "west")
	hour := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	for i := 1; i <= 3; i++ {
		srv["east"].HSet(keys.StatusHour("platform", "api", hour.Unix()), strconv.FormatInt(hour.Add(time.Duration(i)*time.Minute).Unix(), 10), "s")
	}
	rawCheck(srv["west"], hour.Add(time.Minute), true, 400)
	agg, _ := st.computeAggregate(context.Background(), "platform", "api", hour)
	if len(agg.PerLocation) == 0 {
		t.Fatal("no aggregate")
	}
	if agg.AvgMs != 400 {
		t.Fatalf("aggregate latency %v, want 400: east's successes carried no latency", agg.AvgMs)
	}
}

func TestLatencyLeavesOutAnOldRollupHourThatStartsBeforeTheRange(t *testing.T) {
	st, srv := newTestStore(t, "east")
	hour := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	storedRollup(srv["east"], hour, `"east":{"total":60,"success":60,"avgMs":100,"minMs":100,"maxMs":100,"measured":60}`)
	storedRollup(srv["east"], hour.Add(time.Hour), `"east":{"total":60,"success":60,"avgMs":100,"minMs":100,"maxMs":100,"measured":60}`)
	lat, err := st.Latency(context.Background(), "platform", "api", hour.Add(30*time.Minute), hour.Add(5*time.Hour), 1440)
	if err != nil {
		t.Fatal(err)
	}
	if pts := lat.Series["east"]; len(pts) != 1 || pts[0].T != hour.Add(time.Hour).Unix() {
		t.Fatalf("points %+v; want only the 11:00 rollup, not the 10:00 hour that starts before the range", pts)
	}
}
