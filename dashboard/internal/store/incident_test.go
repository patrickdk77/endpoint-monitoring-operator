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

func storeHour(t *testing.T, srv *valkeytest.Server, hour time.Time, perLocation map[string]LocationRollup) {
	t.Helper()
	data, err := json.Marshal(AggregateRollup{PerLocation: perLocation})
	if err != nil {
		t.Fatal(err)
	}
	srv.HSet(keys.RollupAggregateHour("platform", "api"), strconv.FormatInt(hour.Unix(), 10), string(data))
}

func failingHour(hour time.Time, interval time.Duration, msg string) LocationRollup {
	n := int(time.Hour / interval)
	end := hour.Add(time.Duration(n-1) * interval).Unix()
	return LocationRollup{Total: n, Failure: n, Runs: []FailureRun{
		{Start: hour.Unix(), End: end, Count: n, Message: msg, OpenStart: true, OpenEnd: true},
	}}
}

var passingHour = LocationRollup{Total: 6, Success: 6}

func incidents(t *testing.T, st *Store, from, to, now time.Time, live bool) []FailureWindow {
	t.Helper()
	windows, err := st.FailureWindows(context.Background(), "platform", "api", from, to, now, live)
	if err != nil {
		t.Fatal(err)
	}
	return windows
}

func TestAnOutageCheckedEveryTenMinutesIsOneIncidentAcrossHours(t *testing.T) {
	st, srv := newTestStore(t, "east")
	start := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	for h := 0; h < 3; h++ {
		storeHour(t, srv["east"], start.Add(time.Duration(h)*time.Hour), map[string]LocationRollup{
			"east": failingHour(start.Add(time.Duration(h)*time.Hour), 10*time.Minute, "connection refused"),
		})
	}
	now := start.Add(5 * time.Hour)
	w := incidents(t, st, start, now, now, true)
	if len(w) != 1 || w[0].Count != 18 || w[0].Start != start.Unix() || w[0].End != start.Add(2*time.Hour+50*time.Minute).Unix() {
		t.Fatalf("incidents %+v; want one incident of 18 checks from 09:00 to 11:50", w)
	}
}

func TestAnHourOfPassingChecksEndsTheIncident(t *testing.T) {
	st, srv := newTestStore(t, "east")
	start := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	storeHour(t, srv["east"], start, map[string]LocationRollup{"east": failingHour(start, 10*time.Minute, "down")})
	storeHour(t, srv["east"], start.Add(time.Hour), map[string]LocationRollup{"east": passingHour})
	storeHour(t, srv["east"], start.Add(2*time.Hour), map[string]LocationRollup{"east": failingHour(start.Add(2*time.Hour), 10*time.Minute, "down")})
	now := start.Add(5 * time.Hour)
	if w := incidents(t, st, start, now, now, true); len(w) != 2 {
		t.Fatalf("incidents %+v; want two, split by the hour that passed", w)
	}
}

func TestAPassAtTheEndOfTheHourEndsTheIncident(t *testing.T) {
	st, srv := newTestStore(t, "east")
	start := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	storeHour(t, srv["east"], start, map[string]LocationRollup{"east": {Total: 6, Success: 2, Failure: 4, Runs: []FailureRun{
		{Start: start.Unix(), End: start.Add(30 * time.Minute).Unix(), Count: 4, Message: "down", OpenStart: true},
	}}})
	storeHour(t, srv["east"], start.Add(time.Hour), map[string]LocationRollup{"east": failingHour(start.Add(time.Hour), 10*time.Minute, "down")})
	now := start.Add(5 * time.Hour)
	if w := incidents(t, st, start, now, now, true); len(w) != 2 {
		t.Fatalf("incidents %+v; want two, split by the 09:40 and 09:50 passes", w)
	}
}

func TestAPassAtTheStartOfTheHourEndsTheIncident(t *testing.T) {
	st, srv := newTestStore(t, "east")
	start := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	next := start.Add(time.Hour)
	storeHour(t, srv["east"], start, map[string]LocationRollup{"east": failingHour(start, 10*time.Minute, "down")})
	storeHour(t, srv["east"], next, map[string]LocationRollup{"east": {Total: 6, Success: 1, Failure: 5, Runs: []FailureRun{
		{Start: next.Add(10 * time.Minute).Unix(), End: next.Add(50 * time.Minute).Unix(), Count: 5, Message: "down", OpenEnd: true},
	}}})
	now := start.Add(5 * time.Hour)
	if w := incidents(t, st, start, now, now, true); len(w) != 2 {
		t.Fatalf("incidents %+v; want two, split by the 10:00 pass", w)
	}
}

func TestANewMessageStartsANewIncident(t *testing.T) {
	st, srv := newTestStore(t, "east")
	start := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	storeHour(t, srv["east"], start, map[string]LocationRollup{"east": failingHour(start, 10*time.Minute, "connection refused")})
	storeHour(t, srv["east"], start.Add(time.Hour), map[string]LocationRollup{"east": failingHour(start.Add(time.Hour), 10*time.Minute, "HTTP 500")})
	now := start.Add(5 * time.Hour)
	if w := incidents(t, st, start, now, now, true); len(w) != 2 {
		t.Fatalf("incidents %+v; want one per message", w)
	}
}

func TestFailuresAcrossHoursWithoutChecksJoinOnlyWithinStatusMaxAge(t *testing.T) {
	st, srv := newTestStore(t, "east")
	start := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	for _, h := range []int{0, 2, 9} {
		hour := start.Add(time.Duration(h) * time.Hour)
		storeHour(t, srv["east"], hour, map[string]LocationRollup{"east": failingHour(hour, 30*time.Minute, "down")})
	}
	now := start.Add(12 * time.Hour)
	w := incidents(t, st, start, now, now, true)
	if len(w) != 2 || w[1].Start != start.Unix() || w[1].Count != 4 || w[0].Start != start.Add(9*time.Hour).Unix() {
		t.Fatalf("incidents %+v; want 00:00 to 02:30 as one, and 09:00 on its own after a gap of more than 6 hours", w)
	}
}

func TestAnOutageIsOngoingBeforeTheFirstCheckOfTheHour(t *testing.T) {
	st, srv := newTestStore(t, "east")
	hour := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	storeHour(t, srv["east"], hour, map[string]LocationRollup{"east": failingHour(hour, 10*time.Minute, "down")})
	now := hour.Add(time.Hour + 30*time.Second)
	if w := incidents(t, st, hour, now, now, true); len(w) != 1 || !w[0].Ongoing {
		t.Fatalf("incidents %+v; want the outage ongoing at 13:00:30, before 13:00's first check", w)
	}
	if w := incidents(t, st, hour, now, now, false); len(w) != 1 || w[0].Ongoing {
		t.Fatalf("incidents %+v; a view that does not reach the present marked it ongoing", w)
	}
}

func TestAnOutageIsNotOngoingAfterAPassOrOnceItsDataIsOld(t *testing.T) {
	st, srv := newTestStore(t, "east", "west")
	hour := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	storeHour(t, srv["east"], hour, map[string]LocationRollup{"east": failingHour(hour, 10*time.Minute, "down")})
	storeHour(t, srv["east"], hour.Add(time.Hour), map[string]LocationRollup{"east": passingHour, "west": failingHour(hour.Add(time.Hour), 10*time.Minute, "down")})
	now := hour.Add(2*time.Hour + 30*time.Second)
	w := incidents(t, st, hour, now, now, true)
	if len(w) != 2 || w[1].Ongoing || !w[0].Ongoing || w[0].Locations[0] != "west" {
		t.Fatalf("incidents %+v; want east's resolved by its pass and west's ongoing", w)
	}
	now = hour.Add(8 * time.Hour)
	if w := incidents(t, st, hour, now, now, true); len(w) != 2 || w[0].Ongoing {
		t.Fatalf("incidents %+v; want nothing ongoing once the newest data is more than 6 hours old", w)
	}
}
