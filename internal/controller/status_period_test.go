package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	notifierTypes "github.com/patrickdk77/endpoint-monitoring-operator/internal/notifier"
)

func TestSetStatusPeriod(t *testing.T) {
	since := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name         string
		since        metav1.Time
		until        time.Time
		wantChange   string
		wantDuration string
	}{
		{"period", metav1.NewTime(since), since.Add(72*time.Hour + 1500*time.Millisecond), since.String(), "72h0m2s"},
		{"same instant", metav1.NewTime(since), since, since.String(), "0s"},
		{"no previous change", metav1.Time{}, since, "", ""},
		{"no end", metav1.NewTime(since), time.Time{}, "", ""},
		{"end before start", metav1.NewTime(since), since.Add(-time.Second), "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			values := notifierTypes.NoticeValues{LastStatusChange: "stale", StatusTime: "stale"}
			setStatusPeriod(&values, tc.since, tc.until)
			if values.LastStatusChange != tc.wantChange || values.StatusTime != tc.wantDuration {
				t.Fatalf("got %q, %q; want %q, %q", values.LastStatusChange, values.StatusTime, tc.wantChange, tc.wantDuration)
			}
		})
	}
}
