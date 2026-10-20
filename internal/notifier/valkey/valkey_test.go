package valkey

import (
	"strings"
	"testing"

	v1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
)

func TestNewValidatesNamesAndRetention(t *testing.T) {
	days := func(n int) *int { return &n }
	tests := []struct {
		name        string
		cfg         v1.ValkeyConfig
		wantErr     string
		wantSeconds int
	}{
		{"defaults to 90 days", v1.ValkeyConfig{Dashboards: []string{"prod"}}, "", 90 * 86400},
		{"one day", v1.ValkeyConfig{Dashboards: []string{"prod"}, RetentionDays: days(1)}, "", 86400},
		{"zero days", v1.ValkeyConfig{Dashboards: []string{"prod"}, RetentionDays: days(0)}, "retentionDays must be at least 1", 0},
		{"negative days", v1.ValkeyConfig{Dashboards: []string{"prod"}, RetentionDays: days(-1)}, "retentionDays must be at least 1", 0},
		{"dashboard with a colon", v1.ValkeyConfig{Dashboards: []string{"prod", "eu:west"}}, `dashboard name "eu:west"`, 0},
		{"empty dashboard name", v1.ValkeyConfig{Dashboards: []string{""}}, `dashboard name ""`, 0},
		{"name with a colon", v1.ValkeyConfig{Dashboards: []string{"prod"}, Name: "api:v2"}, `name "api:v2" must not contain ':'`, 0},
		{"no dashboards", v1.ValkeyConfig{}, "at least one dashboard", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.Enabled, cfg.Endpoint = true, "127.0.0.1:6379"
			n, err := New(&cfg)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := n.(*ValkeyNotifier).retentionSeconds; got != tc.wantSeconds {
				t.Fatalf("retentionSeconds %d, want %d", got, tc.wantSeconds)
			}
		})
	}
}
