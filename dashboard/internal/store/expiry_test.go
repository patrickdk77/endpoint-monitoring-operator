package store

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/keys"
	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/valkeytest"
)

const day = 24 * time.Hour

func storedAggregate(t *testing.T, srv *valkeytest.Server, hour time.Time) (AggregateRollup, bool) {
	t.Helper()
	raw, ok := srv.HGetAll(keys.RollupAggregateHour("platform", "api"))[strconv.FormatInt(hour.Unix(), 10)]
	if !ok {
		return AggregateRollup{}, false
	}
	var agg AggregateRollup
	if err := json.Unmarshal([]byte(raw), &agg); err != nil {
		t.Fatal(err)
	}
	return agg, true
}

func TestRollupWritesExpirePerFieldFromTheirHourAndDay(t *testing.T) {
	ctx := context.Background()
	st, srv := newTestStore(t, "east")
	now := time.Date(2026, 10, 20, 12, 30, 0, 0, time.UTC)
	hourKey, dailyKey := keys.RollupAggregateHour("platform", "api"), keys.RollupDaily("platform", "api")
	srv["east"].HSet(hourKey, "old", "x")
	srv["east"].SetKeyTTL(hourKey, 7776000)

	hour := now.Truncate(time.Hour).Add(-10 * time.Hour)
	rawCheck(srv["east"], hour.Add(time.Minute), true, 100)
	if res, err := st.RollupHour(ctx, "platform", "api", hour, now, true); err != nil || !res.Written {
		t.Fatalf("RollupHour = %+v, %v; want the hour written", res, err)
	}
	field := strconv.FormatInt(hour.Unix(), 10)
	if ttl, ok := srv["east"].FieldTTL(hourKey, field); !ok || ttl != int64((90*day-10*time.Hour-30*time.Minute)/time.Second) {
		t.Fatalf("hourly field TTL %d, %v; want 90 days from its hour", ttl, ok)
	}
	if _, ok := srv["east"].KeyTTL(hourKey); ok {
		t.Fatal("the key-wide TTL is still set; it would drop every rollup at once")
	}
	if agg, _ := storedAggregate(t, srv["east"], hour); agg.InProgress {
		t.Fatal("an hour rolled up after it ended is marked in progress")
	}
	current := now.Truncate(time.Hour)
	rawCheck(srv["east"], current.Add(time.Minute), true, 100)
	if _, err := st.RollupHour(ctx, "platform", "api", current, now, true); err != nil {
		t.Fatal(err)
	}
	if agg, _ := storedAggregate(t, srv["east"], current); !agg.InProgress {
		t.Fatal("the current hour is not marked in progress")
	}
	if _, err := st.RollupHour(ctx, "platform", "api", current, current.Add(61*time.Minute), true); err != nil {
		t.Fatal(err)
	}
	if agg, _ := storedAggregate(t, srv["east"], current); agg.InProgress {
		t.Fatal("a rewrite after the hour ended left it marked in progress")
	}

	stale := now.Add(-91 * day).Truncate(time.Hour)
	rawCheck(srv["east"], stale.Add(time.Minute), true, 100)
	if res, err := st.RollupHour(ctx, "platform", "api", stale, now, true); err != nil || res.Written {
		t.Fatalf("RollupHour for an hour past the retention = %+v, %v; want nothing written", res, err)
	}
	if _, ok := srv["east"].HGetAll(hourKey)[strconv.FormatInt(stale.Unix(), 10)]; ok {
		t.Fatal("a rollup for an hour past the retention was stored")
	}

	d := now.Truncate(day).AddDate(0, 0, -100)
	if err := st.WriteDailyRollup(ctx, "platform", "api", d, DailyRollup{Success: 1, Total: 1}, now); err != nil {
		t.Fatal(err)
	}
	if ttl, ok := srv["east"].FieldTTL(dailyKey, d.Format("2006-01-02")); !ok || ttl != int64((265*day-12*time.Hour-30*time.Minute)/time.Second) {
		t.Fatalf("daily field TTL %d, %v; want 365 days from its day", ttl, ok)
	}
}

func TestExpireRollupFieldsAgesOutFieldsStoredWithoutTTLs(t *testing.T) {
	st, srv := newTestStore(t, "east")
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	hourKey, dailyKey := keys.RollupAggregateHour("platform", "api"), keys.RollupDaily("platform", "api")
	old, recent := now.Add(-100*day), now.Add(-10*day)
	srv["east"].HSet(hourKey, strconv.FormatInt(old.Unix(), 10), "{}")
	srv["east"].HSet(hourKey, strconv.FormatInt(recent.Unix(), 10), "{}")
	srv["east"].HSet(hourKey, "junk", "{}")
	srv["east"].SetKeyTTL(hourKey, 7776000)
	srv["east"].HSet(dailyKey, now.AddDate(0, 0, -400).Format("2006-01-02"), "{}")
	srv["east"].HSet(dailyKey, now.AddDate(0, 0, -100).Format("2006-01-02"), "{}")
	srv["east"].HSet(dailyKey, "junk", "{}")

	if err := st.ExpireRollupFields(context.Background(), "platform", "api", now); err != nil {
		t.Fatal(err)
	}
	hourly := srv["east"].HGetAll(hourKey)
	if _, ok := hourly[strconv.FormatInt(old.Unix(), 10)]; ok {
		t.Error("an hourly rollup 100 days old was kept past the 90 day retention")
	}
	if ttl, ok := srv["east"].FieldTTL(hourKey, strconv.FormatInt(recent.Unix(), 10)); !ok || ttl != int64(80*day/time.Second) {
		t.Errorf("the 10 day old hourly rollup has TTL %d, %v; want 80 days", ttl, ok)
	}
	if _, ok := hourly["junk"]; !ok {
		t.Error("an hourly field that is not an hour was removed")
	}
	if _, ok := srv["east"].KeyTTL(hourKey); ok {
		t.Error("the key-wide TTL is still set")
	}
	daily := srv["east"].HGetAll(dailyKey)
	if _, ok := daily[now.AddDate(0, 0, -400).Format("2006-01-02")]; ok {
		t.Error("a daily rollup 400 days old was kept past 365 days")
	}
	if ttl, ok := srv["east"].FieldTTL(dailyKey, now.AddDate(0, 0, -100).Format("2006-01-02")); !ok || ttl != int64((265*day-12*time.Hour)/time.Second) {
		t.Errorf("the 100 day old daily rollup has TTL %d, %v; want 265 days from its midnight", ttl, ok)
	}
	if _, ok := daily["junk"]; !ok {
		t.Error("a field that is not a date was removed")
	}
}

func TestRollupFieldTTLFailuresKeepTheKeyWideTTL(t *testing.T) {
	ctx := context.Background()
	st, srv := newTestStore(t, "east")
	now := time.Date(2026, 10, 20, 12, 30, 0, 0, time.UTC)
	hourKey := keys.RollupAggregateHour("platform", "api")
	hour := now.Truncate(time.Hour)
	rawCheck(srv["east"], hour.Add(time.Minute), true, 100)
	srv["east"].HSet(hourKey, strconv.FormatInt(hour.Add(-time.Hour).Unix(), 10), "{}")
	srv["east"].SetKeyTTL(hourKey, 7776000)
	srv["east"].Fail("HEXPIRE", ":rollup:agg:hour", "ERR unknown command 'HEXPIRE'")

	if _, err := st.RollupHour(ctx, "platform", "api", hour, now, true); err == nil {
		t.Error("RollupHour succeeded although HEXPIRE failed")
	}
	if err := st.ExpireRollupFields(ctx, "platform", "api", now); err == nil {
		t.Error("ExpireRollupFields succeeded although HEXPIRE failed")
	}
	if _, ok := srv["east"].KeyTTL(hourKey); !ok {
		t.Fatal("the key-wide TTL was removed although no field got its own")
	}
	srv["east"].Heal()
	srv["east"].Fail("HKEYS", ":rollup:agg:hour", "ERR primary is restarting")
	if err := st.ExpireRollupFields(ctx, "platform", "api", now); err == nil {
		t.Error("ExpireRollupFields succeeded although HKEYS failed")
	}
}

func TestRollupHourStoresWhatItCanReadAndKeepsTheRest(t *testing.T) {
	ctx := context.Background()
	st, srv := newTestStore(t, "east", "west")
	hour := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	now := hour.Add(90 * time.Minute)
	rawCheck(srv["east"], hour.Add(time.Minute), true, 100)
	rawCheck(srv["west"], hour.Add(time.Minute), false, 100)
	if _, err := st.RollupHour(ctx, "platform", "api", hour, now, true); err != nil {
		t.Fatal(err)
	}

	rawCheck(srv["east"], hour.Add(2*time.Minute), true, 100)
	srv["west"].Fail("HGETALL", ":s:", "ERR west is down")
	res, err := st.RollupHour(ctx, "platform", "api", hour, now, true)
	if err != nil || !res.Written || res.Missing["west"] == nil || len(res.Missing) != 1 {
		t.Fatalf("RollupHour = %+v, %v; want the hour written with west missing", res, err)
	}
	agg, _ := storedAggregate(t, srv["east"], hour)
	if agg.Success != 2 || agg.Failure != 1 || len(agg.Missing) != 1 || agg.Missing[0] != "west" || agg.PerLocation["west"].Failure != 1 {
		t.Fatalf("stored %+v; want east's two checks, west's stored failure kept, and west listed as missing", agg)
	}

	srv["west"].Heal()
	if _, err := st.RollupHour(ctx, "platform", "api", hour, now, true); err != nil {
		t.Fatal(err)
	}
	if agg, _ := storedAggregate(t, srv["east"], hour); len(agg.Missing) != 0 || agg.Success != 2 || agg.Failure != 1 {
		t.Fatalf("stored %+v; want the hour complete once west answers", agg)
	}
}

func TestRollupHourStoresAnHourWhileALocationIsDown(t *testing.T) {
	st, srv := newTestStore(t, "east", "west")
	hour := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	rawCheck(srv["east"], hour.Add(time.Minute), false, 100)
	srv["west"].Fail("HGETALL", ":s:", "ERR west is down")
	if _, err := st.RollupHour(context.Background(), "platform", "api", hour, hour.Add(90*time.Minute), true); err != nil {
		t.Fatal(err)
	}
	if agg, ok := storedAggregate(t, srv["east"], hour); !ok || agg.Failure != 1 || len(agg.Missing) != 1 {
		t.Fatalf("stored %+v, %v; want east's failure stored with west missing", agg, ok)
	}
}

func TestRollupHourKeepsCountsWhoseRawSamplesExpired(t *testing.T) {
	st, srv := newTestStore(t, "east", "west")
	hour := time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC)
	srv["east"].HSet(keys.RollupAggregateHour("platform", "api"), strconv.FormatInt(hour.Unix(), 10),
		`{"status":"major_outage","failure":6,"perLocation":{"east":{"total":6,"failure":6,"status":"outage"}},"missing":["west"]}`)
	rawCheck(srv["west"], hour.Add(time.Minute), true, 100)
	rawCheck(srv["west"], hour.Add(2*time.Minute), true, 100)
	if _, err := st.RollupHour(context.Background(), "platform", "api", hour, hour.Add(10*day), true); err != nil {
		t.Fatal(err)
	}
	agg, _ := storedAggregate(t, srv["east"], hour)
	if agg.Failure != 6 || agg.Success != 2 || len(agg.Missing) != 0 {
		t.Fatalf("stored %+v; want east's six stored failures kept beside west's two checks", agg)
	}
}

func TestRollupHourWritesNothingWhenTheStoredRollupCannotBeRead(t *testing.T) {
	st, srv := newTestStore(t, "east")
	hour := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	storedRollup(srv["east"], hour, `"east":{"total":6,"failure":6,"status":"outage"}`)
	rawCheck(srv["east"], hour.Add(time.Minute), true, 100)
	srv["east"].Fail("HGET", ":rollup:agg:hour", "ERR primary is restarting")
	if res, err := st.RollupHour(context.Background(), "platform", "api", hour, hour.Add(90*time.Minute), true); err == nil || res.Written {
		t.Fatalf("RollupHour = %+v, %v; want an error and nothing written", res, err)
	}
	if agg, _ := storedAggregate(t, srv["east"], hour); agg.PerLocation["east"].Failure != 6 {
		t.Fatalf("stored %+v; the stored rollup was overwritten", agg)
	}
}

func TestRollupHourMarksAFinishedHourItCouldNotRead(t *testing.T) {
	ctx := context.Background()
	st, srv := newTestStore(t, "east", "west")
	hour := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	rawCheck(srv["west"], hour.Add(time.Minute), false, 100)
	rawCheck(srv["west"], hour.Add(time.Hour+time.Minute), false, 100)
	srv["west"].Fail("HGETALL", ":s:", "ERR west is down")

	if _, err := st.RollupHour(ctx, "platform", "api", hour.Add(time.Hour), hour.Add(90*time.Minute), true); err != nil {
		t.Fatal(err)
	}
	if agg, ok := storedAggregate(t, srv["east"], hour.Add(time.Hour)); ok {
		t.Fatalf("the current hour was stored as %+v with nothing readable; a later pass still rolls it up", agg)
	}
	if _, err := st.RollupHour(ctx, "platform", "api", hour, hour.Add(90*time.Minute), true); err != nil {
		t.Fatal(err)
	}
	agg, ok := storedAggregate(t, srv["east"], hour)
	if !ok || len(agg.PerLocation) != 0 || len(agg.Missing) != 1 || agg.Status != "unknown" {
		t.Fatalf("the finished hour = %+v, %v; want it stored as unknown with west missing", agg, ok)
	}

	srv["west"].Heal()
	if _, err := st.RollupHour(ctx, "platform", "api", hour, hour.Add(150*time.Minute), true); err != nil {
		t.Fatal(err)
	}
	if agg, _ := storedAggregate(t, srv["east"], hour); agg.Failure != 1 || len(agg.Missing) != 0 {
		t.Fatalf("the hour once west answered = %+v; want west's failure", agg)
	}
}

func TestAnEmptyHoursMarkerIsClearedOnceEveryLocationAnswers(t *testing.T) {
	ctx := context.Background()
	st, srv := newTestStore(t, "east", "west")
	hour := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	now := hour.Add(65 * time.Minute)
	srv["west"].Fail("HGETALL", ":s:", "ERR west is down")
	if res, err := st.RollupHour(ctx, "platform", "api", hour, now, true); err != nil || !res.Written {
		t.Fatalf("RollupHour = %+v, %v; want the empty hour marked", res, err)
	}
	if res, err := st.RollupHour(ctx, "platform", "api", hour, now.Add(5*time.Minute), true); err != nil || res.Written {
		t.Fatalf("RollupHour = %+v, %v; want the same marker left as it is", res, err)
	}

	srv["west"].Heal()
	if _, err := st.RollupHour(ctx, "platform", "api", hour, now.Add(10*time.Minute), true); err != nil {
		t.Fatal(err)
	}
	if agg, ok := storedAggregate(t, srv["east"], hour); ok {
		t.Fatalf("the marker %+v stayed after every location answered with no checks", agg)
	}
}

func TestOnlyAnEmptyHoursMarkerIsDeleted(t *testing.T) {
	st, srv := newTestStore(t, "east")
	hour := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	srv["east"].HSet(keys.RollupAggregateHour("platform", "api"), strconv.FormatInt(hour.Unix(), 10),
		`{"status":"degraded","success":1,"failure":1}`)
	if _, err := st.RollupHour(context.Background(), "platform", "api", hour, hour.Add(2*time.Hour), true); err != nil {
		t.Fatal(err)
	}
	if agg, ok := storedAggregate(t, srv["east"], hour); !ok || agg.Failure != 1 {
		t.Fatalf("a stored hour without per-location counts became %+v, %v; want it kept", agg, ok)
	}
}
