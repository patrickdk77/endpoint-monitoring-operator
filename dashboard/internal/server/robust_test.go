package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/config"
	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/keys"
	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/store"
	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/valkeytest"
)

func newTestServer(t *testing.T) (http.Handler, *valkeytest.Server) {
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
	s, err := New(st, 90)
	if err != nil {
		t.Fatal(err)
	}
	srv.SAdd(keys.Dashboards(), "platform")
	srv.SAdd(keys.Services("platform"), "api")
	srv.HSet(keys.Meta("platform", "api"), "endpoint", "https://api.example/")
	srv.HSet(keys.Meta("platform", "api"), "driver", "http")
	return s.Handler(), srv
}

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	body, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(body)
}

func seedSample(srv *valkeytest.Server, at time.Time, ok bool) {
	hour := at.UTC().Truncate(time.Hour).Unix()
	ts := strconv.FormatInt(at.Unix(), 10)
	st := "s"
	if !ok {
		st = "f"
	}
	srv.HSet(keys.StatusHour("platform", "api", hour), ts, st)
	srv.HSet(keys.DetailHour("platform", "api", hour), ts, "10")
}

func TestPagesAre503WhenTheRollupReadFails(t *testing.T) {
	h, srv := newTestServer(t)
	seedSample(srv, time.Now(), true)
	srv.Fail("HGETALL", ":rollup:agg:hour", "ERR primary is restarting")
	for _, path := range []string{"/platform", "/platform/api", "/api/dashboard/platform/overview", "/api/dashboard/platform/api"} {
		if code, body := get(t, h, path); code != http.StatusServiceUnavailable {
			t.Errorf("GET %s = %d %q, want 503", path, code, body)
		}
	}
}

func TestPagesAre503WhenEveryLocationIsDown(t *testing.T) {
	h, srv := newTestServer(t)
	srv.Fail("SMEMBERS", "emo:", "ERR east is down")
	for _, path := range []string{"/platform", "/api/dashboards", "/api/dashboard/platform/overview"} {
		if code, body := get(t, h, path); code != http.StatusServiceUnavailable {
			t.Errorf("GET %s = %d %q, want 503", path, code, body)
		}
	}
}

func TestAServiceWithNoDataIsUnknown(t *testing.T) {
	h, _ := newTestServer(t)
	code, body := get(t, h, "/platform")
	if code != http.StatusOK {
		t.Fatalf("GET /platform = %d", code)
	}
	if !strings.Contains(body, `class="status status-unknown"`) {
		t.Fatalf("a service with no data does not show as unknown:\n%s", body)
	}
}

func TestUnavailableDoesNotEchoTheStoreError(t *testing.T) {
	h, srv := newTestServer(t)
	srv.Fail("HGETALL", ":rollup:agg:hour", "ERR secret internal detail")
	code, body := get(t, h, "/api/dashboard/platform/api")
	if code != http.StatusServiceUnavailable || strings.Contains(body, "secret internal detail") ||
		!strings.Contains(body, "status data is unavailable") {
		t.Fatalf("GET /api/dashboard/platform/api = %d %q; want a 503 with the generic body only", code, body)
	}
}

func TestStatusIgnoresDataOlderThanSixHours(t *testing.T) {
	h, srv := newTestServer(t)
	rollup := func(ago time.Duration) {
		hour := time.Now().UTC().Add(-ago).Truncate(time.Hour).Unix()
		srv.HSet(keys.RollupAggregateHour("platform", "api"), strconv.FormatInt(hour, 10),
			`{"status":"degraded","success":1,"failure":1}`)
	}
	rollup(8 * time.Hour)
	if _, body := get(t, h, "/platform"); !strings.Contains(body, `class="status status-unknown"`) {
		t.Fatalf("a service whose newest data is 8 hours old does not show as unknown:\n%s", body)
	}
	rollup(3 * time.Hour)
	if _, body := get(t, h, "/platform"); !strings.Contains(body, `class="status status-degraded"`) {
		t.Fatalf("a service with a degraded hour 3 hours ago does not show as degraded:\n%s", body)
	}
}

func TestAServiceWithRecentDataShowsItsStatus(t *testing.T) {
	h, srv := newTestServer(t)
	seedSample(srv, time.Now(), true)
	_, body := get(t, h, "/platform")
	if !strings.Contains(body, `class="status status-operational"`) {
		t.Fatalf("a service with a successful check this hour does not show as operational:\n%s", body)
	}
}

func TestASingleLocationWithOneFailedCheckIsDegraded(t *testing.T) {
	h, srv := newTestServer(t)
	hour := time.Now().UTC().Truncate(time.Hour)
	for i := 0; i < 60; i++ {
		seedSample(srv, hour.Add(time.Duration(i)*time.Second), i != 30)
	}
	if _, body := get(t, h, "/platform"); !strings.Contains(body, `class="status status-degraded"`) {
		t.Fatalf("one failed check in 60 at the only location does not show as degraded:\n%s", body)
	}
}
