package store

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/config"
	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/keys"
	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/status"
	"github.com/valkey-io/valkey-go"
)

type LocationRollup struct {
	Total    int     `json:"total"`
	Success  int     `json:"success"`
	Failure  int     `json:"failure"`
	AvgMs    float64 `json:"avgMs"`
	Status   string  `json:"status"`
	Messages []FailureMessage `json:"messages,omitempty"`
}

type FailureMessage struct {
	Timestamp int64  `json:"ts"`
	Ms        int64  `json:"ms"`
	Message   string `json:"message"`
}

type AggregateRollup struct {
	Status       string                     `json:"status"`
	Success      int                        `json:"success"`
	Failure      int                        `json:"failure"`
	AvgMs        float64                    `json:"avgMs"`
	PerLocation  map[string]LocationRollup  `json:"perLocation"`
}

type Meta struct {
	Endpoint string `json:"endpoint"`
	Driver   string `json:"driver"`
	Name     string `json:"name"`
}

type Client struct {
	name   string
	client valkey.Client
}

type Store struct {
	locations map[string]*Client
	primary   *Client
	retention int
}

func New(cfg *config.Config) (*Store, error) {
	s := &Store{
		locations: make(map[string]*Client),
		retention: cfg.DefaultRetentionDays * 24 * 3600,
	}
	for _, loc := range cfg.Locations {
		c, err := dial(loc)
		if err != nil {
			return nil, fmt.Errorf("location %s: %w", loc.Name, err)
		}
		client := &Client{name: loc.Name, client: c}
		s.locations[loc.Name] = client
		if loc.Primary {
			s.primary = client
		}
	}
	if s.primary == nil {
		return nil, fmt.Errorf("no primary location configured")
	}
	return s, nil
}

func dial(loc config.Location) (valkey.Client, error) {
	opt := valkey.ClientOption{
		InitAddress: []string{loc.Addr},
		Username:    loc.Username,
		Password:    loc.Password,
		SelectDB:    loc.DB,
	}
	if loc.TLS {
		opt.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	return valkey.NewClient(opt)
}

func (s *Store) Close() {
	for _, c := range s.locations {
		c.client.Close()
	}
}

func (s *Store) ListDashboards(ctx context.Context) ([]string, error) {
	seen := map[string]struct{}{}
	var out []string
	for _, c := range s.locations {
		items, err := c.client.Do(ctx, c.client.B().Smembers().Key(keys.Dashboards()).Build()).AsStrSlice()
		if err != nil {
			continue
		}
		for _, d := range items {
			if _, ok := seen[d]; ok {
				continue
			}
			seen[d] = struct{}{}
			out = append(out, d)
		}
	}
	return out, nil
}

func (s *Store) ListServices(ctx context.Context, dash string) ([]string, error) {
	seen := map[string]struct{}{}
	var out []string
	for _, c := range s.locations {
		items, err := c.client.Do(ctx, c.client.B().Smembers().Key(keys.Services(dash)).Build()).AsStrSlice()
		if err != nil {
			continue
		}
		for _, svc := range items {
			if _, ok := seen[svc]; ok {
				continue
			}
			seen[svc] = struct{}{}
			out = append(out, svc)
		}
	}
	return out, nil
}

func (s *Store) GetMeta(ctx context.Context, dash, svc string) (Meta, error) {
	for _, c := range s.locations {
		m, err := c.client.Do(ctx, c.client.B().Hgetall().Key(keys.Meta(dash, svc)).Build()).AsStrMap()
		if err != nil || len(m) == 0 {
			continue
		}
		return Meta{
			Endpoint: m["endpoint"],
			Driver:   m["driver"],
			Name:     m["name"],
		}, nil
	}
	return Meta{}, fmt.Errorf("meta not found for %s/%s", dash, svc)
}

func (s *Store) ComputeLocationRollup(ctx context.Context, locName, dash, svc string, hour time.Time) (LocationRollup, error) {
	c := s.locations[locName]
	if c == nil {
		return LocationRollup{}, fmt.Errorf("unknown location %s", locName)
	}
	hourEpoch := hour.Unix()
	statusMap, err := c.client.Do(ctx, c.client.B().Hgetall().Key(keys.StatusHour(dash, svc, hourEpoch)).Build()).AsStrMap()
	if err != nil {
		return LocationRollup{}, err
	}
	detailMap, err := c.client.Do(ctx, c.client.B().Hgetall().Key(keys.DetailHour(dash, svc, hourEpoch)).Build()).AsStrMap()
	if err != nil {
		return LocationRollup{}, err
	}
	if len(statusMap) == 0 {
		return LocationRollup{}, nil
	}

	var successes, failures int
	var totalMs int64
	var samples int
	var messages []FailureMessage

	for ts, st := range statusMap {
		switch st {
		case "s":
			successes++
		case "f":
			failures++
		}
		if detail, ok := detailMap[ts]; ok {
			ms, msg := parseDetail(detail)
			totalMs += ms
			samples++
			if msg != "" {
				tsInt, _ := strconv.ParseInt(ts, 10, 64)
				messages = append(messages, FailureMessage{Timestamp: tsInt, Ms: ms, Message: msg})
			}
		}
	}

	rollup := LocationRollup{
		Total:    successes + failures,
		Success:  successes,
		Failure:  failures,
		Messages: messages,
		Status:   string(status.LocationFromCounts(successes, failures)),
	}
	if samples > 0 {
		rollup.AvgMs = float64(totalMs) / float64(samples)
	}
	return rollup, nil
}

func parseDetail(detail string) (int64, string) {
	parts := splitDetail(detail)
	ms, _ := strconv.ParseInt(parts[0], 10, 64)
	if len(parts) > 1 {
		return ms, parts[1]
	}
	return ms, ""
}

func splitDetail(detail string) []string {
	for i := 0; i < len(detail); i++ {
		if detail[i] == '|' {
			return []string{detail[:i], detail[i+1:]}
		}
	}
	return []string{detail}
}

// ComputeAggregate reads raw samples for the given hour from every location
// (read-only) and computes the per-location rollups plus the cross-location
// aggregate status. It performs no writes. The bool is false when no location
// has any data for the hour.
func (s *Store) ComputeAggregate(ctx context.Context, dash, svc string, hour time.Time) (AggregateRollup, bool) {
	perLocation := make(map[string]LocationRollup)
	var locStatuses []status.LocationStatus
	var totalSuccess, totalFailure int
	var totalMs float64
	var msSamples int

	for _, loc := range s.LocationNames() {
		lr, err := s.ComputeLocationRollup(ctx, loc, dash, svc, hour)
		if err != nil || lr.Total == 0 {
			continue
		}
		perLocation[loc] = lr
		locStatuses = append(locStatuses, status.LocationStatus(lr.Status))
		totalSuccess += lr.Success
		totalFailure += lr.Failure
		if lr.AvgMs > 0 {
			totalMs += lr.AvgMs
			msSamples++
		}
	}

	if len(locStatuses) == 0 {
		return AggregateRollup{}, false
	}

	agg := AggregateRollup{
		Status:      string(status.AggregateFromLocations(locStatuses)),
		Success:     totalSuccess,
		Failure:     totalFailure,
		PerLocation: perLocation,
	}
	if msSamples > 0 {
		agg.AvgMs = totalMs / float64(msSamples)
	}
	return agg, true
}

func (s *Store) WriteLocationRollup(ctx context.Context, dash, svc, loc string, hour time.Time, rollup LocationRollup) error {
	if rollup.Total == 0 {
		return nil
	}
	data, err := json.Marshal(rollup)
	if err != nil {
		return err
	}
	field := strconv.FormatInt(hour.Unix(), 10)
	key := keys.RollupLocationHour(dash, svc, loc)
	if err := s.primary.client.Do(ctx, s.primary.client.B().Hset().Key(key).FieldValue().FieldValue(field, string(data)).Build()).Error(); err != nil {
		return err
	}
	return s.primary.client.Do(ctx, s.primary.client.B().Expire().Key(key).Seconds(int64(s.retention)).Build()).Error()
}

func (s *Store) WriteAggregateRollup(ctx context.Context, dash, svc string, hour time.Time, rollup AggregateRollup) error {
	data, err := json.Marshal(rollup)
	if err != nil {
		return err
	}
	field := strconv.FormatInt(hour.Unix(), 10)
	key := keys.RollupAggregateHour(dash, svc)
	if err := s.primary.client.Do(ctx, s.primary.client.B().Hset().Key(key).FieldValue().FieldValue(field, string(data)).Build()).Error(); err != nil {
		return err
	}
	return s.primary.client.Do(ctx, s.primary.client.B().Expire().Key(key).Seconds(int64(s.retention)).Build()).Error()
}

func (s *Store) GetAggregateRollups(ctx context.Context, dash, svc string, from, to time.Time) (map[int64]AggregateRollup, error) {
	key := keys.RollupAggregateHour(dash, svc)
	all, err := s.primary.client.Do(ctx, s.primary.client.B().Hgetall().Key(key).Build()).AsStrMap()
	if err != nil {
		return nil, err
	}
	out := make(map[int64]AggregateRollup)
	for field, val := range all {
		hourEpoch, err := strconv.ParseInt(field, 10, 64)
		if err != nil {
			continue
		}
		t := time.Unix(hourEpoch, 0).UTC()
		if t.Before(from) || t.After(to) {
			continue
		}
		var rollup AggregateRollup
		if err := json.Unmarshal([]byte(val), &rollup); err != nil {
			continue
		}
		out[hourEpoch] = rollup
	}
	return out, nil
}

func (s *Store) LocationNames() []string {
	names := make([]string, 0, len(s.locations))
	for name := range s.locations {
		names = append(names, name)
	}
	return names
}
