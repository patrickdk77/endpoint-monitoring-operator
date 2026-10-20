package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/keys"
	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/store"
)

func TestLatencyAPIUsesTheChosenRangeAndFallsBackTo24Hours(t *testing.T) {
	h, srv := newTestServer(t)
	seedSample(srv, time.Now().Add(-time.Minute), true)
	for _, tc := range []struct {
		query string
		days  int64
	}{
		{"", 1},
		{"?range=7d", 7},
		{"?range=30d", 30},
		{"?range=90d", 90},
		{"?range=365d", 1},
		{"?range=bogus", 1},
	} {
		code, body := get(t, h, "/api/dashboard/platform/api/latency"+tc.query)
		if code != http.StatusOK {
			t.Fatalf("GET latency%s = %d %q", tc.query, code, body)
		}
		var lat store.Latency
		if err := json.Unmarshal([]byte(body), &lat); err != nil {
			t.Fatal(err)
		}
		if lat.To-lat.From != tc.days*86400 {
			t.Errorf("latency%s covers %ds, want %d days", tc.query, lat.To-lat.From, tc.days)
		}
		if len(lat.Series["east"]) != 1 {
			t.Errorf("latency%s has %d points for east, want the one check", tc.query, len(lat.Series["east"]))
		}
	}
}

func TestLatencyAPIIs503WhenTheRollupReadFails(t *testing.T) {
	h, srv := newTestServer(t)
	srv.Fail("HGETALL", ":rollup:agg:hour", "ERR primary is restarting")
	if code, body := get(t, h, "/api/dashboard/platform/api/latency"); code != http.StatusServiceUnavailable {
		t.Fatalf("GET latency = %d %q, want 503", code, body)
	}
}

func TestRangeOptionsFollowTheRetention(t *testing.T) {
	for _, tc := range []struct {
		retention int
		want      string
	}{
		{90, "1d 7d 30d 90d"},
		{30, "1d 7d 30d"},
		{10, "1d 7d 10d"},
		{7, "1d 7d"},
		{1, "1d"},
	} {
		s := &Server{retention: tc.retention}
		var got []string
		for _, o := range s.rangeOptions() {
			got = append(got, o.Key)
		}
		if strings.Join(got, " ") != tc.want {
			t.Errorf("retention %d: ranges %v, want %s", tc.retention, got, tc.want)
		}
	}
}

func TestIssuesFollowTheChosenRange(t *testing.T) {
	h, srv := newTestServer(t)
	hour := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Hour)
	start := hour.Add(10 * time.Minute).Unix()
	srv.HSet(keys.RollupAggregateHour("platform", "api"), strconv.FormatInt(hour.Unix(), 10),
		`{"status":"degraded","success":10,"failure":2,"perLocation":{"east":{"total":12,"success":10,"failure":2,`+
			`"runs":[{"start":`+strconv.FormatInt(start, 10)+`,"end":`+strconv.FormatInt(start+60, 10)+`,"count":2,"message":"three days ago"}]}}}`)

	_, body := get(t, h, "/platform/api")
	if strings.Contains(body, "three days ago") || !strings.Contains(body, "Issues (last 24 hours)") {
		t.Fatalf("the default 24 hour view shows a 3 day old incident or the wrong title:\n%s", body)
	}
	_, body = get(t, h, "/platform/api?range=7d")
	if !strings.Contains(body, "three days ago") || !strings.Contains(body, "Issues (last 7 days)") {
		t.Fatalf("the 7 day view misses the 3 day old incident or its title:\n%s", body)
	}
	if !strings.Contains(body, `href="/platform/api?range=7d" class="active">7 days</a>`) {
		t.Fatalf("the 7 day range link is not marked active:\n%s", body)
	}
	if !strings.Contains(body, `<option value="90d">90 days</option>`) {
		t.Fatalf("the chart range list does not offer the full retention:\n%s", body)
	}
}

func TestTheDayRangePlotsEveryCheckAndLongerRangesAreCapped(t *testing.T) {
	h, srv := newTestServer(t)
	now := time.Now().UTC()
	for i := 0; i < 2880; i++ {
		seedSample(srv, now.Add(-time.Duration(i)*30*time.Second-time.Second), true)
	}
	for _, tc := range []struct {
		query    string
		points   int
		bucketed bool
	}{
		{"?range=1d", 2880, false},
		{"?range=7d", 1440, true},
	} {
		_, body := get(t, h, "/api/dashboard/platform/api/latency"+tc.query)
		var lat store.Latency
		if err := json.Unmarshal([]byte(body), &lat); err != nil {
			t.Fatal(err)
		}
		n := len(lat.Series["east"])
		if (lat.Bucket > 0) != tc.bucketed || (tc.bucketed && n > tc.points) || (!tc.bucketed && n != tc.points) {
			t.Errorf("latency%s: %d points, bucket %d; want %d points, bucketed %v", tc.query, n, lat.Bucket, tc.points, tc.bucketed)
		}
	}
}

func TestTheChartRangeFollowsTheIssuesRange(t *testing.T) {
	h, _ := newTestServer(t)
	_, body := get(t, h, "/platform/api?range=7d")
	if !strings.Contains(body, `<option value="7d" selected>7 days</option>`) {
		t.Fatalf("the chart does not preselect the 7 day range:\n%s", body)
	}
}
