package server

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/keys"
)

func TestCardShowsTheThirtyDayUptimeRoundedDown(t *testing.T) {
	h, srv := newTestServer(t)
	hour := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	srv.HSet(keys.RollupAggregateHour("platform", "api"), strconv.FormatInt(hour.Unix(), 10),
		`{"status":"operational","success":99996}`)
	srv.HSet(keys.RollupAggregateHour("platform", "api"), strconv.FormatInt(hour.AddDate(0, 0, -20).Unix(), 10),
		`{"status":"major_outage","failure":4}`)
	_, body := get(t, h, "/platform")
	if !strings.Contains(body, `<span class="sla-num">99.99</span>`) {
		t.Fatalf("4 failures in 100000 checks do not show as 99.99:\n%s", body)
	}
	if !strings.Contains(body, `title="Uptime over the last 30 days"`) {
		t.Fatalf("the card SLA is not labelled as 30 days:\n%s", body)
	}
}

func TestServicePageRendersTheSLAsOnTheServer(t *testing.T) {
	h, srv := newTestServer(t)
	now := time.Now().UTC()
	srv.HSet(keys.RollupAggregateHour("platform", "api"), strconv.FormatInt(now.Truncate(time.Hour).Add(-time.Hour).Unix(), 10),
		`{"status":"operational","success":10}`)
	srv.HSet(keys.RollupDaily("platform", "api"), now.AddDate(0, 0, -15).Format("2006-01-02"),
		`{"success":0,"failure":10,"total":10}`)
	srv.HSet(keys.RollupDaily("platform", "api"), now.AddDate(0, 0, -200).Format("2006-01-02"),
		`{"success":0,"failure":10,"total":10}`)
	_, body := get(t, h, "/platform/api")
	for id, want := range map[string]string{"sla7": "100", "sla30": "50", "sla365": "33.33"} {
		if !strings.Contains(body, `id="`+id+`"><span class="sla-`) || !strings.Contains(body, `id="`+id+`"><span class="`+slaClassFor(want)+`"><span class="sla-num">`+want+`</span>`) {
			t.Errorf("%s does not render %s%%:\n%s", id, want, body)
		}
	}
}

func slaClassFor(num string) string {
	if num == "100" {
		return "sla-good"
	}
	return "sla-danger"
}
