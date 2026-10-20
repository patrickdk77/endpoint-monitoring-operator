package server

import (
	"html/template"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/keys"
)

func TestUptimeColorFollowsTheNinesScale(t *testing.T) {
	for _, tc := range []struct {
		success, total int
		want           template.CSS
	}{
		{1440, 1440, "var(--operational)"},
		{1999, 2000, "color-mix(in srgb, var(--degraded) 25.0%, var(--operational))"},
		{999, 1000, "color-mix(in srgb, var(--degraded) 50.0%, var(--operational))"},
		{99, 100, "color-mix(in srgb, var(--degraded) 100.0%, var(--operational))"},
		{97, 100, "color-mix(in srgb, var(--major_outage) 25.0%, var(--degraded))"},
		{95, 100, "color-mix(in srgb, var(--major_outage) 50.0%, var(--degraded))"},
		{90, 100, "color-mix(in srgb, var(--major_outage) 100.0%, var(--degraded))"},
		{1, 2, "color-mix(in srgb, var(--major_outage) 100.0%, var(--degraded))"},
	} {
		if got := uptimeColor(tc.success, tc.total); got != tc.want {
			t.Errorf("uptimeColor(%d, %d) = %q, want %q", tc.success, tc.total, got, tc.want)
		}
	}
}

func TestDayBarsAreShadedByTheirOwnUptime(t *testing.T) {
	h, srv := newTestServer(t)
	yesterday := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
	srv.HSet(keys.RollupAggregateHour("platform", "api"), strconv.FormatInt(yesterday.Add(10*time.Hour).Unix(), 10),
		`{"status":"degraded","success":1439,"failure":1}`)
	date := yesterday.Format("2006-01-02")
	for _, path := range []string{"/platform", "/platform/api"} {
		_, body := get(t, h, path)
		if !strings.Contains(body, ` bar-shaded" style="--shade: color-mix(in srgb, var(--degraded) 34.7%, var(--operational))"`) {
			t.Errorf("%s: yesterday's bar is not shaded for 99.93%% uptime:\n%s", path, body)
		}
		if !strings.Contains(body, `title="`+date+`: 99.93% (1 of 1440 failed)`) {
			t.Errorf("%s: yesterday's bar does not show its uptime on hover:\n%s", path, body)
		}
		if !strings.Contains(body, `: no data`) {
			t.Errorf("%s: a day without checks does not say so on hover", path)
		}
		if strings.Count(body, `--shade:`) != 1 {
			t.Errorf("%s: %d bars are shaded; want only yesterday's, the one day with checks", path, strings.Count(body, `--shade:`))
		}
	}
}

func TestTheShadeAppliesOnlyWhereColorMixWorks(t *testing.T) {
	h, _ := newTestServer(t)
	_, css := get(t, h, "/static/style.css")
	if !strings.Contains(css, "@supports (background: color-mix(in srgb, red 50%, blue)) {\n  .bar-shaded { background: var(--shade); }\n}") {
		t.Fatalf("the stylesheet applies the shade without checking for color-mix():\n%s", css)
	}
}
