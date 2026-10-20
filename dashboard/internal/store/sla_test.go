package store

import (
	"context"
	"testing"
	"time"

	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/keys"
)

func TestFormatPercentRoundsDownAndKeeps100ForNoFailures(t *testing.T) {
	for _, tc := range []struct {
		success, total int
		want           string
	}{
		{0, 0, ""},
		{10, 10, "100%"},
		{99996, 100000, "99.99%"},
		{525595, 525600, "99.99%"},
		{999, 1000, "99.9%"},
		{2, 3, "66.66%"},
		{1, 3, "33.33%"},
		{0, 5, "0%"},
		{9995, 10000, "99.95%"},
	} {
		if got := FormatPercent(tc.success, tc.total); got != tc.want {
			t.Errorf("FormatPercent(%d, %d) = %q, want %q", tc.success, tc.total, got, tc.want)
		}
	}
}

func TestUptimePrefersAFinishedDaysDailyRollup(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 30, 0, 0, time.UTC)
	hour := func(daysAgo, h int) int64 {
		return now.Truncate(24*time.Hour).AddDate(0, 0, -daysAgo).Add(time.Duration(h) * time.Hour).Unix()
	}
	day := func(daysAgo int) string {
		return now.AddDate(0, 0, -daysAgo).Format("2006-01-02")
	}
	hist := History{
		Hours: map[int64]AggregateRollup{
			hour(0, 1):  {Success: 10},
			hour(0, 2):  {Success: 9, Failure: 1},
			hour(1, 5):  {Success: 1000},
			hour(2, 5):  {Success: 7, Failure: 3},
			hour(7, 23): {Failure: 50},
		},
		Days: map[string]DailyRollup{
			day(0):   {Success: 1, Failure: 1},
			day(1):   {Success: 4, Failure: 1},
			day(200): {Success: 90, Failure: 10},
		},
	}
	for _, tc := range []struct {
		days           int
		success, total int
	}{
		{1, 19, 20},
		{2, 23, 25},
		{3, 30, 35},
		{7, 30, 35},
		{8, 30, 85},
		{365, 120, 185},
	} {
		success, total := hist.Uptime(tc.days, now)
		if success != tc.success || total != tc.total {
			t.Errorf("Uptime(%d) = %d/%d, want %d/%d", tc.days, success, total, tc.success, tc.total)
		}
	}
}

func TestHistoryLoadsTheDailyRollups(t *testing.T) {
	st, srv := newTestStore(t, "east")
	now := time.Date(2026, 10, 20, 12, 30, 0, 0, time.UTC)
	srv["east"].HSet(keys.RollupDaily("platform", "api"), now.AddDate(0, 0, -200).Format("2006-01-02"), `{"success":90,"failure":10,"total":100}`)
	srv["east"].HSet(keys.RollupDaily("platform", "api"), now.AddDate(0, 0, -400).Format("2006-01-02"), `{"success":1,"total":1}`)
	srv["east"].HSet(keys.RollupDaily("platform", "api"), now.AddDate(0, 0, -365).Format("2006-01-02"), `{"success":2,"total":2}`)
	srv["east"].HSet(keys.RollupDaily("platform", "api"), now.AddDate(0, 0, -366).Format("2006-01-02"), `{"success":3,"total":3}`)
	hist, err := st.History(context.Background(), "platform", "api", now)
	if err != nil {
		t.Fatal(err)
	}
	if dr, ok := hist.Days[now.AddDate(0, 0, -200).Format("2006-01-02")]; !ok || dr.Success != 90 || dr.Failure != 10 {
		t.Errorf("History.Days missing the rollup from 200 days ago: %+v", hist.Days)
	}
	if _, ok := hist.Days[now.AddDate(0, 0, -400).Format("2006-01-02")]; ok {
		t.Errorf("History.Days includes a rollup from 400 days ago")
	}
	if _, ok := hist.Days[now.AddDate(0, 0, -365).Format("2006-01-02")]; !ok {
		t.Errorf("History.Days left out the rollup from 365 days ago")
	}
	if _, ok := hist.Days[now.AddDate(0, 0, -366).Format("2006-01-02")]; ok {
		t.Errorf("History.Days includes a rollup from 366 days ago")
	}
}
