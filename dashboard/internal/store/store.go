package store

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/config"
	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/keys"
	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/status"
	"github.com/valkey-io/valkey-go"
)

type LocationRollup struct {
	Total   int          `json:"total"`
	Success int          `json:"success"`
	Failure int          `json:"failure"`
	AvgMs   float64      `json:"avgMs"`
	Status  string       `json:"status"`
	Runs    []FailureRun `json:"runs,omitempty"`
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
		if smp.hasDetail {
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
// true, the current hour is recomputed on the fly and a run that is still open
// at the latest sample of the current hour is marked Ongoing.
func (s *Store) FailureWindows(ctx context.Context, dash, svc string, from, to time.Time, live bool) []FailureWindow {
	rollups, _ := s.GetAggregateRollups(ctx, dash, svc, from, to)

	curEpoch := int64(-1)
	curHour := time.Now().UTC().Truncate(time.Hour)
	if live && !curHour.Before(from.Truncate(time.Hour)) && !curHour.After(to) {
		curEpoch = curHour.Unix()
		if agg, ok := s.ComputeAggregate(ctx, dash, svc, curHour); ok {
			rollups[curEpoch] = agg
		}
	}

	hours := make([]int64, 0, len(rollups))
	for h := range rollups {
		hours = append(hours, h)
	}
	sort.Slice(hours, func(i, j int) bool { return hours[i] < hours[j] })

	segByLoc := map[string][]segment{}
	prevOpenEnd := map[string]bool{}
	lastEpoch := map[string]int64{}
	for _, h := range hours {
		for loc, lr := range rollups[h].PerLocation {
			for _, run := range lr.Runs {
				segs := segByLoc[loc]
				stitch := len(segs) > 0 && prevOpenEnd[loc] && run.OpenStart &&
					segs[len(segs)-1].msg == run.Message &&
					run.Start-segs[len(segs)-1].end <= mergeGapSeconds
				if stitch {
					segs[len(segs)-1].end = run.End
					segs[len(segs)-1].count += run.Count
				} else {
					segs = append(segs, segment{loc: loc, msg: run.Message, start: run.Start, end: run.End, count: run.Count})
				}
				segByLoc[loc] = segs
				prevOpenEnd[loc] = run.OpenEnd
				lastEpoch[loc] = h
			}
		}
	}

	var all []segment
	for loc, segs := range segByLoc {
		// A location's most recent run is ongoing only if it is still open at
		// the latest sample of the current hour.
		if live && curEpoch >= 0 && lastEpoch[loc] == curEpoch && prevOpenEnd[loc] && len(segs) > 0 {
			segs[len(segs)-1].ongoing = true
		}
		all = append(all, segs...)
	}

	return mergeSegments(all)
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

// WriteDailyRollup stores a single day's rollup in the primary store and
// expires it after 365 days.
func (s *Store) WriteDailyRollup(ctx context.Context, dash, svc string, day time.Time, dr DailyRollup) error {
	data, err := json.Marshal(dr)
	if err != nil {
		return err
	}
	field := day.Format("2006-01-02")
	key := keys.RollupDaily(dash, svc)
	if err := s.primary.client.Do(ctx, s.primary.client.B().Hset().Key(key).FieldValue().FieldValue(field, string(data)).Build()).Error(); err != nil {
		return err
	}
	// expire after 365 days
	return s.primary.client.Do(ctx, s.primary.client.B().Expire().Key(key).Seconds(int64(365*24*3600)).Build()).Error()
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
// single daily summary. If no data exists for the day, nothing is written.
func (s *Store) UpdateDailyRollup(ctx context.Context, dash, svc string, day time.Time) error {
	from := day.Truncate(24 * time.Hour)
	to := from.Add(24*time.Hour - time.Second)
	hours, err := s.GetAggregateRollups(ctx, dash, svc, from, to)
	if err != nil {
		return err
	}
	daily := computeDailyFromHours(hours)
	fld := from.Format("2006-01-02")
	if dr, ok := daily[fld]; ok && dr.Total > 0 {
		return s.WriteDailyRollup(ctx, dash, svc, from, dr)
	}
	return nil
}

// EnsureYesterdayDaily computes yesterday's daily rollup. Called frequently
// from the rollup runner; it's safe to call repeatedly.
func (s *Store) EnsureYesterdayDaily(ctx context.Context, dash, svc string) error {
	y := time.Now().UTC().AddDate(0, 0, -1).Truncate(24 * time.Hour)
	return s.UpdateDailyRollup(ctx, dash, svc, y)
}

// ComputeSLA computes the uptime percentage over the last `days` calendar
// days (inclusive). Returns an empty string when there is no data.
func (s *Store) ComputeSLA(ctx context.Context, dash, svc string, days int) (string, error) {
	now := time.Now().UTC()
	fromDay := now.AddDate(0, 0, -(days - 1)).Truncate(24 * time.Hour)

	// Preferentially compute SLA from stored daily rollups (keeps a year's
	// worth of daily summaries); fall back to hourly aggregates when daily
	// rollups are unavailable.
	dailyMap, err := s.GetDailyRollups(ctx, dash, svc, fromDay, now)
	if err == nil && len(dailyMap) > 0 {
		var success, failure int
		for _, dr := range dailyMap {
			success += dr.Success
			failure += dr.Failure
		}
		// Overlay current day's partial data (hours) if it's within the window
		today := now.Truncate(24 * time.Hour)
		if !today.Before(fromDay) {
			hours, _ := s.GetAggregateRollups(ctx, dash, svc, today, now)
			for _, r := range hours {
				success += r.Success
				failure += r.Failure
			}
		}
		total := success + failure
		if total == 0 {
			return "", nil
		}
		return fmt.Sprintf("%.2f%%", float64(success)/float64(total)*100), nil
	}

	// Fallback: compute from hourly aggregate rollups across the requested
	// window (same behavior as before).
	from := fromDay
	rollups, err := s.GetAggregateRollups(ctx, dash, svc, from, now)
	if err != nil {
		return "", err
	}
	curHour := now.Truncate(time.Hour)
	if agg, ok := s.ComputeAggregate(ctx, dash, svc, curHour); ok {
		rollups[curHour.Unix()] = agg
	}
	var success, failure int
	for _, r := range rollups {
		success += r.Success
		failure += r.Failure
	}
	total := success + failure
	if total == 0 {
		return "", nil
	}
	return fmt.Sprintf("%.2f%%", float64(success)/float64(total)*100), nil
}

// UpdateSLAs computes 7/30/365 day SLAs and stores them as fields on the
// service meta hash (fields sla7, sla30, sla365). Errors are returned when
// the underlying store operations fail.
func (s *Store) UpdateSLAs(ctx context.Context, dash, svc string) error {
	sla7, err := s.ComputeSLA(ctx, dash, svc, 7)
	if err != nil {
		return err
	}
	sla30, err := s.ComputeSLA(ctx, dash, svc, 30)
	if err != nil {
		return err
	}
	sla365, err := s.ComputeSLA(ctx, dash, svc, 365)
	if err != nil {
		return err
	}

	key := keys.Meta(dash, svc)
	// Hset multiple fields
	if err := s.primary.client.Do(ctx, s.primary.client.B().Hset().Key(key).FieldValue().FieldValue("sla7", sla7).FieldValue().FieldValue("sla30", sla30).FieldValue().FieldValue("sla365", sla365).Build()).Error(); err != nil {
		return err
	}
	return nil
}
