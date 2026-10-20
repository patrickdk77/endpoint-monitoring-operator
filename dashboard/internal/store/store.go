package store

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/config"
	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/keys"
	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/status"
	"github.com/valkey-io/valkey-go"
)

type LocationRollup struct {
	Total    int          `json:"total"`
	Success  int          `json:"success"`
	Failure  int          `json:"failure"`
	AvgMs    float64      `json:"avgMs"`
	MinMs    float64      `json:"minMs,omitempty"`
	MaxMs    float64      `json:"maxMs,omitempty"`
	Measured int          `json:"measured,omitempty"`
	Status   string       `json:"status"`
	Runs     []FailureRun `json:"runs,omitempty"`
}

// FailureRun is a maximal run of consecutive failing samples within a single
// hour that share the same message. OpenStart/OpenEnd indicate the run touches
// the first/last sample of the hour, so it may be a continuation of the
// previous hour or continue into the next one; this lets the reader stitch runs
// across hour boundaries without losing success-driven breaks inside an hour.
type FailureRun struct {
	Start     int64  `json:"start"`
	End       int64  `json:"end"`
	Count     int    `json:"count"`
	Message   string `json:"message"`
	OpenStart bool   `json:"openStart,omitempty"`
	OpenEnd   bool   `json:"openEnd,omitempty"`
}

type AggregateRollup struct {
	Status      string                    `json:"status"`
	Success     int                       `json:"success"`
	Failure     int                       `json:"failure"`
	AvgMs       float64                   `json:"avgMs"`
	PerLocation map[string]LocationRollup `json:"perLocation"`
	Missing     []string                  `json:"missing,omitempty"`
	InProgress  bool                      `json:"inProgress,omitempty"`
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
	locations     map[string]*Client
	primary       *Client
	retention     int
	retentionDays int
}

func New(cfg *config.Config) (*Store, error) {
	s := &Store{
		locations:     make(map[string]*Client),
		retention:     cfg.DefaultRetentionDays * 24 * 3600,
		retentionDays: cfg.DefaultRetentionDays,
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
	return s.union(ctx, keys.Dashboards())
}

func (s *Store) ListServices(ctx context.Context, dash string) ([]string, error) {
	return s.union(ctx, keys.Services(dash))
}

// union merges one set across every location and fails only when every
// location does.
func (s *Store) union(ctx context.Context, key string) ([]string, error) {
	seen := map[string]struct{}{}
	var out []string
	var errs []error
	for _, name := range s.LocationNames() {
		c := s.locations[name]
		items, err := c.client.Do(ctx, c.client.B().Smembers().Key(key).Build()).AsStrSlice()
		if err != nil {
			errs = append(errs, fmt.Errorf("location %s: %w", name, err))
			continue
		}
		for _, item := range items {
			if _, ok := seen[item]; ok {
				continue
			}
			seen[item] = struct{}{}
			out = append(out, item)
		}
	}
	if len(errs) == len(s.locations) {
		return nil, fmt.Errorf("reading %s: %w", key, errors.Join(errs...))
	}
	for _, err := range errs {
		log.Printf("store: reading %s: %v", key, err)
	}
	return out, nil
}

// GetMeta returns the first meta hash, reading the primary and then the other
// locations by name, that names an endpoint or a driver.
func (s *Store) GetMeta(ctx context.Context, dash, svc string) (Meta, error) {
	order := []*Client{s.primary}
	for _, name := range s.LocationNames() {
		if c := s.locations[name]; c != s.primary {
			order = append(order, c)
		}
	}
	var errs []error
	for _, c := range order {
		m, err := c.client.Do(ctx, c.client.B().Hgetall().Key(keys.Meta(dash, svc)).Build()).AsStrMap()
		if err != nil {
			errs = append(errs, fmt.Errorf("location %s: %w", c.name, err))
			continue
		}
		if m["endpoint"] == "" && m["driver"] == "" {
			continue
		}
		return Meta{
			Endpoint: m["endpoint"],
			Driver:   m["driver"],
			Name:     m["name"],
		}, nil
	}
	if len(errs) > 0 {
		return Meta{}, fmt.Errorf("meta for %s/%s: %w", dash, svc, errors.Join(errs...))
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

	type lsample struct {
		ts        int64
		fail      bool
		ms        int64
		msg       string
		hasDetail bool
	}
	samplesList := make([]lsample, 0, len(statusMap))
	for tss, st := range statusMap {
		tsi, err := strconv.ParseInt(tss, 10, 64)
		if err != nil {
			continue
		}
		smp := lsample{ts: tsi, fail: st == "f"}
		if d, ok := detailMap[tss]; ok {
			smp.ms, smp.msg = parseDetail(d)
			smp.hasDetail = true
		}
		samplesList = append(samplesList, smp)
	}
	sort.Slice(samplesList, func(i, j int) bool { return samplesList[i].ts < samplesList[j].ts })

	var successes, failures int
	var totalMs int64
	var msSamples int
	var minMs, maxMs int64
	var runs []FailureRun
	var curRun FailureRun
	open := false
	flush := func(openEnd bool) {
		if open {
			curRun.OpenEnd = openEnd
			runs = append(runs, curRun)
			open = false
		}
	}
	for idx, smp := range samplesList {
		if smp.hasDetail && !smp.fail {
			if msSamples == 0 || smp.ms < minMs {
				minMs = smp.ms
			}
			if smp.ms > maxMs {
				maxMs = smp.ms
			}
			totalMs += smp.ms
			msSamples++
		}
		if smp.fail {
			failures++
			if open && curRun.Message == smp.msg {
				curRun.End = smp.ts
				curRun.Count++
			} else {
				flush(false)
				curRun = FailureRun{Start: smp.ts, End: smp.ts, Count: 1, Message: smp.msg, OpenStart: idx == 0}
				open = true
			}
		} else {
			successes++
			flush(false)
		}
	}
	flush(true)

	rollup := LocationRollup{
		Total:   successes + failures,
		Success: successes,
		Failure: failures,
		Runs:    runs,
		Status:  string(status.LocationFromCounts(successes, failures)),
	}
	if msSamples > 0 {
		rollup.AvgMs = float64(totalMs) / float64(msSamples)
		rollup.MinMs = float64(minMs)
		rollup.MaxMs = float64(maxMs)
		rollup.Measured = msSamples
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
// aggregate status. It performs no writes. A location that could not be read
// is listed in Missing. The bool is false when no location has any data for
// the hour.
func (s *Store) ComputeAggregate(ctx context.Context, dash, svc string, hour time.Time) (AggregateRollup, bool) {
	agg, _ := s.computeAggregate(ctx, dash, svc, hour)
	return agg, len(agg.PerLocation) > 0
}

// computeAggregate is ComputeAggregate that also returns why each missing
// location could not be read.
func (s *Store) computeAggregate(ctx context.Context, dash, svc string, hour time.Time) (AggregateRollup, map[string]error) {
	perLocation := make(map[string]LocationRollup)
	var missing []string
	var failed map[string]error
	for _, loc := range s.LocationNames() {
		lr, err := s.ComputeLocationRollup(ctx, loc, dash, svc, hour)
		if err != nil {
			if failed == nil {
				failed = map[string]error{}
			}
			failed[loc] = err
			missing = append(missing, loc)
			continue
		}
		if lr.Total > 0 {
			perLocation[loc] = lr
		}
	}
	agg := aggregate(perLocation)
	agg.Missing = missing
	return agg, failed
}

// aggregate sums one hour's per-location rollups.
func aggregate(perLocation map[string]LocationRollup) AggregateRollup {
	names := make([]string, 0, len(perLocation))
	for name := range perLocation {
		names = append(names, name)
	}
	sort.Strings(names)
	agg := AggregateRollup{PerLocation: perLocation, Status: aggregateStatus(perLocation)}
	var totalMs float64
	var msSamples int
	for _, name := range names {
		lr := perLocation[name]
		agg.Success += lr.Success
		agg.Failure += lr.Failure
		if lr.Measured > 0 {
			totalMs += lr.AvgMs * float64(lr.Measured)
			msSamples += lr.Measured
		}
	}
	if msSamples > 0 {
		agg.AvgMs = totalMs / float64(msSamples)
	}
	return agg
}

func aggregateStatus(perLocation map[string]LocationRollup) string {
	statuses := make([]status.LocationStatus, 0, len(perLocation))
	for _, lr := range perLocation {
		statuses = append(statuses, status.LocationFromCounts(lr.Success, lr.Failure))
	}
	return string(status.AggregateFromLocations(statuses))
}

// dailyRetention is how long a daily rollup is kept.
const dailyRetention = 365 * 24 * time.Hour

// HourRollup reports what RollupHour did with one service-hour: whether it
// stored a rollup, and why each location it could not read failed.
type HourRollup struct {
	Written bool
	Missing map[string]error
}

// RollupHour computes the hour's rollup from every location's raw samples and
// stores it until the hour is older than the retention. A location that could
// not be read is listed in the rollup's Missing and keeps what the stored
// rollup holds for it, and every location keeps its stored counts when they
// are higher, as they are once its raw samples have expired. A rollup written
// before its hour ends is marked InProgress. With mark set, a finished hour
// with no readable data and a location that could not be read is stored with
// no locations, so a later backfill finds it; such an hour is deleted once
// every location answers with no data. The error covers reading the stored rollup and writing the new
// one; nothing is written when the stored rollup cannot be read.
func (s *Store) RollupHour(ctx context.Context, dash, svc string, hour, now time.Time, mark bool) (HourRollup, error) {
	expiresAt := hour.Add(time.Duration(s.retention) * time.Second)
	if !expiresAt.After(now) {
		return HourRollup{}, nil
	}
	agg, failed := s.computeAggregate(ctx, dash, svc, hour)
	res := HourRollup{Missing: failed}
	key, field := keys.RollupAggregateHour(dash, svc), strconv.FormatInt(hour.Unix(), 10)
	c := s.primary.client
	stored, err := c.Do(ctx, c.B().Hget().Key(key).Field(field).Build()).ToString()
	if err != nil && !valkey.IsValkeyNil(err) {
		return res, err
	}
	var old AggregateRollup
	if err == nil && json.Unmarshal([]byte(stored), &old) == nil {
		for loc, lr := range old.PerLocation {
			if lr.Total > agg.PerLocation[loc].Total {
				agg.PerLocation[loc] = lr
			}
		}
	}
	inProgress := now.Before(hour.Add(time.Hour))
	placeholder := len(old.Missing) > 0 && len(old.PerLocation) == 0
	if len(agg.PerLocation) == 0 {
		if placeholder && len(agg.Missing) == 0 {
			return res, c.Do(ctx, c.B().Hdel().Key(key).Field(field).Build()).Error()
		}
		if len(agg.Missing) == 0 || inProgress || !mark || (placeholder && slices.Equal(old.Missing, agg.Missing)) {
			return res, nil
		}
	}
	rollup := aggregate(agg.PerLocation)
	rollup.Missing = agg.Missing
	rollup.InProgress = inProgress
	data, err := json.Marshal(rollup)
	if err != nil {
		return res, err
	}
	res.Written, err = s.writeExpiring(ctx, key, field, string(data), expiresAt, now)
	return res, err
}

// writeExpiring stores one hash field on the primary with its own TTL, ending
// at expiresAt, then removes any key-wide TTL, which would drop every field at
// once. It writes nothing for a field already past expiresAt, and keeps the
// key-wide TTL when the field's TTL could not be set.
func (s *Store) writeExpiring(ctx context.Context, key, field, value string, expiresAt, now time.Time) (bool, error) {
	ttl := int64(expiresAt.Sub(now) / time.Second)
	if ttl <= 0 {
		return false, nil
	}
	c := s.primary.client
	for _, resp := range c.DoMulti(ctx,
		c.B().Hset().Key(key).FieldValue().FieldValue(field, value).Build(),
		c.B().Hexpire().Key(key).Seconds(ttl).Fields().Numfields(1).Field(field).Build(),
	) {
		if err := resp.Error(); err != nil {
			return false, err
		}
	}
	return true, c.Do(ctx, c.B().Persist().Key(key).Build()).Error()
}

// ExpireRollupFields gives every stored hourly and daily rollup field of a
// service the TTL it would have had under per-field expiry, and deletes those
// already past it. Rollups stored before per-field expiry only had a
// key-wide TTL that each write renewed, so none of them ever expired.
func (s *Store) ExpireRollupFields(ctx context.Context, dash, svc string, now time.Time) error {
	hourly := func(field string) (time.Time, bool) {
		epoch, err := strconv.ParseInt(field, 10, 64)
		if err != nil {
			return time.Time{}, false
		}
		return time.Unix(epoch, 0).Add(time.Duration(s.retention) * time.Second), true
	}
	if err := s.expireFields(ctx, keys.RollupAggregateHour(dash, svc), now, hourly); err != nil {
		return err
	}
	daily := func(field string) (time.Time, bool) {
		day, err := time.Parse("2006-01-02", field)
		if err != nil {
			return time.Time{}, false
		}
		return day.Add(dailyRetention), true
	}
	return s.expireFields(ctx, keys.RollupDaily(dash, svc), now, daily)
}

func (s *Store) expireFields(ctx context.Context, key string, now time.Time, expiry func(string) (time.Time, bool)) error {
	c := s.primary.client
	fields, err := c.Do(ctx, c.B().Hkeys().Key(key).Build()).AsStrSlice()
	if err != nil {
		return err
	}
	var cmds []valkey.Completed
	for _, field := range fields {
		at, ok := expiry(field)
		if !ok {
			continue
		}
		if ttl := int64(at.Sub(now) / time.Second); ttl > 0 {
			cmds = append(cmds, c.B().Hexpire().Key(key).Seconds(ttl).Fields().Numfields(1).Field(field).Build())
		} else {
			cmds = append(cmds, c.B().Hdel().Key(key).Field(field).Build())
		}
	}
	if len(cmds) == 0 {
		return nil
	}
	for _, resp := range c.DoMulti(ctx, cmds...) {
		if err := resp.Error(); err != nil {
			return err
		}
	}
	return c.Do(ctx, c.B().Persist().Key(key).Build()).Error()
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
		if len(rollup.PerLocation) > 0 {
			rollup.Status = aggregateStatus(rollup.PerLocation)
		}
		out[hourEpoch] = rollup
	}
	return out, nil
}

// History holds one service's hourly aggregate rollups from the start of the
// day retentionDays ago, with the current hour computed live from the samples,
// and its daily rollups for the last 366 days.
type History struct {
	Hours map[int64]AggregateRollup
	Days  map[string]DailyRollup
}

func (s *Store) History(ctx context.Context, dash, svc string, now time.Time) (History, error) {
	from := now.AddDate(0, 0, -s.retentionDays).Truncate(24 * time.Hour)
	hours, err := s.GetAggregateRollups(ctx, dash, svc, from, now)
	if err != nil {
		return History{}, err
	}
	cur := now.Truncate(time.Hour)
	if agg, ok := s.ComputeAggregate(ctx, dash, svc, cur); ok {
		hours[cur.Unix()] = agg
	}
	days, err := s.GetDailyRollups(ctx, dash, svc, now.AddDate(0, 0, -365), now)
	if err != nil {
		return History{}, err
	}
	return History{Hours: hours, Days: days}, nil
}

// FailureWindow is a failure incident: a run of failing samples sharing the
// same message. It is built per location, then per-location runs that share the
// message and overlap (or are near-concurrent) in time are merged into a single
// incident that lists every location reporting it.
type FailureWindow struct {
	Locations []string `json:"locations"`
	Message   string   `json:"message"`
	Start     int64    `json:"start"`
	End       int64    `json:"end"`
	Count     int      `json:"count"`
	Ongoing   bool     `json:"ongoing"`
}

// mergeGapSeconds is the tolerance for treating two same-message location runs
// as the same incident, absorbing sampling-offset differences between locations.
const mergeGapSeconds = int64(300)

// StatusMaxAge is how old a service's newest data may be for the dashboard to
// still show its status. A location's failures more than this far apart are
// separate incidents, and a run whose newest hour is older is not ongoing.
const StatusMaxAge = 6 * time.Hour

type segment struct {
	loc     string
	msg     string
	start   int64
	end     int64
	count   int
	ongoing bool
}

// FailureWindows derives failure incidents from the stored per-hour rollups
// (the precomputed per-location FailureRuns) over [from, to]. It stitches each
// location's hourly runs back into continuous runs across hour boundaries, then
// merges same-message runs that overlap in time into incidents listing every
// reporting location. Incidents are returned most-recent-first. When live is
// true, the current hour is recomputed on the fly, and a location's run that
// holds its newest sample is marked Ongoing while that sample's hour is within
// StatusMaxAge of now.
func (s *Store) FailureWindows(ctx context.Context, dash, svc string, from, to, now time.Time, live bool) ([]FailureWindow, error) {
	rollups, err := s.GetAggregateRollups(ctx, dash, svc, from, to)
	if err != nil {
		return nil, err
	}

	curHour := now.Truncate(time.Hour)
	if live && !curHour.Before(from.Truncate(time.Hour)) && !curHour.After(to) {
		if agg, ok := s.ComputeAggregate(ctx, dash, svc, curHour); ok {
			rollups[curHour.Unix()] = agg
		}
	}

	hours := make([]int64, 0, len(rollups))
	for h := range rollups {
		hours = append(hours, h)
	}
	sort.Slice(hours, func(i, j int) bool { return hours[i] < hours[j] })

	segByLoc := map[string][]segment{}
	open := map[string]bool{}
	lastHour := map[string]int64{}
	maxGap := int64(StatusMaxAge / time.Second)
	for _, h := range hours {
		for loc, lr := range rollups[h].PerLocation {
			segs := segByLoc[loc]
			for _, run := range lr.Runs {
				stitch := open[loc] && run.OpenStart &&
					segs[len(segs)-1].msg == run.Message &&
					run.Start-segs[len(segs)-1].end <= maxGap
				if stitch {
					segs[len(segs)-1].end = run.End
					segs[len(segs)-1].count += run.Count
				} else {
					segs = append(segs, segment{loc: loc, msg: run.Message, start: run.Start, end: run.End, count: run.Count})
				}
			}
			segByLoc[loc] = segs
			open[loc] = len(lr.Runs) > 0 && lr.Runs[len(lr.Runs)-1].OpenEnd
			lastHour[loc] = h
		}
	}

	var all []segment
	recent := now.Add(-StatusMaxAge).Unix()
	for loc, segs := range segByLoc {
		if live && open[loc] && lastHour[loc] >= recent {
			segs[len(segs)-1].ongoing = true
		}
		all = append(all, segs...)
	}

	return mergeSegments(all), nil
}

// mergeSegments groups per-location runs by message and merges those whose time
// ranges overlap (within mergeGapSeconds) into a single incident.
func mergeSegments(segments []segment) []FailureWindow {
	byMsg := map[string][]segment{}
	for _, sg := range segments {
		byMsg[sg.msg] = append(byMsg[sg.msg], sg)
	}

	var windows []FailureWindow
	for msg, segs := range byMsg {
		sort.Slice(segs, func(i, j int) bool { return segs[i].start < segs[j].start })
		for i := 0; i < len(segs); {
			win := FailureWindow{Message: msg, Start: segs[i].start, End: segs[i].end, Count: segs[i].count, Ongoing: segs[i].ongoing}
			locs := map[string]struct{}{segs[i].loc: {}}
			j := i + 1
			for j < len(segs) && segs[j].start <= win.End+mergeGapSeconds {
				if segs[j].end > win.End {
					win.End = segs[j].end
				}
				win.Count += segs[j].count
				win.Ongoing = win.Ongoing || segs[j].ongoing
				locs[segs[j].loc] = struct{}{}
				j++
			}
			win.Locations = sortedKeys(locs)
			windows = append(windows, win)
			i = j
		}
	}

	sort.Slice(windows, func(i, j int) bool {
		if windows[i].Start != windows[j].Start {
			return windows[i].Start > windows[j].Start
		}
		return windows[i].Message < windows[j].Message
	})
	return windows
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (s *Store) LocationNames() []string {
	names := make([]string, 0, len(s.locations))
	for name := range s.locations {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// DailyRollup represents a per-day success/failure summary used to compute
// higher-window SLAs without retaining all hourly data for long periods.
type DailyRollup struct {
	Date    string `json:"date"`
	Success int    `json:"success"`
	Failure int    `json:"failure"`
	Total   int    `json:"total"`
}

// WriteDailyRollup stores a single day's rollup in the primary store until the
// day is 365 days old.
func (s *Store) WriteDailyRollup(ctx context.Context, dash, svc string, day time.Time, dr DailyRollup, now time.Time) error {
	data, err := json.Marshal(dr)
	if err != nil {
		return err
	}
	_, err = s.writeExpiring(ctx, keys.RollupDaily(dash, svc), day.Format("2006-01-02"), string(data), day.Add(dailyRetention), now)
	return err
}

// GetDailyRollups reads stored daily rollups over [from, to].
func (s *Store) GetDailyRollups(ctx context.Context, dash, svc string, from, to time.Time) (map[string]DailyRollup, error) {
	key := keys.RollupDaily(dash, svc)
	all, err := s.primary.client.Do(ctx, s.primary.client.B().Hgetall().Key(key).Build()).AsStrMap()
	if err != nil {
		return nil, err
	}
	out := make(map[string]DailyRollup)
	for field, val := range all {
		t, err := time.Parse("2006-01-02", field)
		if err != nil {
			continue
		}
		tt := t.UTC()
		if tt.Before(from.Truncate(24*time.Hour)) || tt.After(to.Truncate(24*time.Hour)) {
			continue
		}
		var dr DailyRollup
		if err := json.Unmarshal([]byte(val), &dr); err != nil {
			continue
		}
		out[field] = dr
	}
	return out, nil
}

// computeDailyFromHours converts hourly aggregate rollups into daily
// summaries (map keyed by YYYY-MM-DD).
func computeDailyFromHours(rollups map[int64]AggregateRollup) map[string]DailyRollup {
	byDay := map[string]DailyRollup{}
	for h, r := range rollups {
		day := time.Unix(h, 0).UTC().Format("2006-01-02")
		dr := byDay[day]
		dr.Date = day
		dr.Success += r.Success
		dr.Failure += r.Failure
		dr.Total = dr.Success + dr.Failure
		byDay[day] = dr
	}
	return byDay
}

// UpdateDailyRollup computes and writes the daily rollup for the provided day
// (UTC day). It reads hourly aggregate rollups for that day and writes a
// single daily summary, and reports whether it wrote one. If no data exists
// for the day, nothing is written.
func (s *Store) UpdateDailyRollup(ctx context.Context, dash, svc string, day, now time.Time) (bool, error) {
	from := day.Truncate(24 * time.Hour)
	to := from.Add(24*time.Hour - time.Second)
	hours, err := s.GetAggregateRollups(ctx, dash, svc, from, to)
	if err != nil {
		return false, err
	}
	daily := computeDailyFromHours(hours)
	fld := from.Format("2006-01-02")
	if dr, ok := daily[fld]; ok && dr.Total > 0 {
		err := s.WriteDailyRollup(ctx, dash, svc, from, dr, now)
		return err == nil, err
	}
	return false, nil
}

// FormatPercent renders success/total as a percentage rounded down to at most
// two decimals, without trailing zeros. It shows 100% only when nothing
// failed, and an empty string when there were no checks.
func FormatPercent(success, total int) string {
	if total <= 0 {
		return ""
	}
	if success >= total {
		return "100%"
	}
	bp := int64(success) * 10000 / int64(total)
	num := strings.TrimRight(fmt.Sprintf("%d.%02d", bp/100, bp%100), "0")
	return strings.TrimRight(num, ".") + "%"
}

// Uptime counts the checks over the given number of UTC days ending today:
// today so far and the days-1 days before it. A finished day counts from its
// daily rollup when there is one, and from its hourly rollups otherwise.
func (h History) Uptime(days int, now time.Time) (success, total int) {
	type counts struct{ success, total int }
	hourly := map[int64]counts{}
	for epoch, r := range h.Hours {
		day := time.Unix(epoch, 0).UTC().Truncate(24 * time.Hour).Unix()
		c := hourly[day]
		c.success += r.Success
		c.total += r.Success + r.Failure
		hourly[day] = c
	}
	today := now.UTC().Truncate(24 * time.Hour)
	for d := 0; d < days; d++ {
		day := today.AddDate(0, 0, -d)
		if dr, ok := h.Days[day.Format("2006-01-02")]; ok && d > 0 {
			success += dr.Success
			total += dr.Success + dr.Failure
			continue
		}
		c := hourly[day.Unix()]
		success += c.success
		total += c.total
	}
	return success, total
}
