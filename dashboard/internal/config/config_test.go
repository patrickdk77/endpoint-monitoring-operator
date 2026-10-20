package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func load(t *testing.T, yaml string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

const twoLocations = `
locations:
  - name: east
    addr: east:6379
    primary: true
  - name: west
    addr: west:6379
`

func TestLoadAcceptsAValidConfig(t *testing.T) {
	cfg, err := load(t, "rollupInterval: 1m\nbackfillPace: 0s\n"+twoLocations)
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := cfg.RollupDuration(); d.Minutes() != 1 {
		t.Fatalf("RollupDuration = %v, want 1m", d)
	}
}

func TestLoadRejectsBadIntervalsAndLocations(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, wantErr string
	}{
		{"zero rollup interval", "rollupInterval: 0s\n" + twoLocations, "rollupInterval must be a positive duration"},
		{"negative rollup interval", "rollupInterval: -5m\n" + twoLocations, "rollupInterval must be a positive duration"},
		{"unparsable rollup interval", "rollupInterval: often\n" + twoLocations, "rollupInterval"},
		{"negative backfill pace", "backfillPace: -1s\n" + twoLocations, "backfillPace must not be negative"},
		{"negative retention", "defaultRetentionDays: -3\n" + twoLocations, "defaultRetentionDays must be at least 1"},
		{"unparsable backfill pace", "backfillPace: slow\n" + twoLocations, "backfillPace"},
		{"duplicate location", `
locations:
  - name: east
    addr: east:6379
    primary: true
  - name: east
    addr: east2:6379
`, `location name "east" is used twice`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := load(t, tc.yaml)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Load = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}
