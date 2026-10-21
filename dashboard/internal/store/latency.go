package store

import (
	"context"
	"log"
	"sort"
	"strconv"
	"time"

	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/keys"
	"github.com/valkey-io/valkey-go"
)

// Point is one plotted latency value: a single check, an hour taken from a
// rollup, or a bucket of either when the range holds more entries than the
// point limit. Avg, Min and Max cover the successful checks only and are nil
// when none succeeded. Span is the number of seconds the point covers, 0 for
// a single check.
type Point struct {
	T      int64    `json:"t"`
	Span   int64    `json:"span,omitempty"`
	Avg    *float64 `json:"avg"`
	Min    *float64 `json:"min"`
	Max    *float64 `json:"max"`
	Checks int      `json:"checks"`
	Failed int      `json:"failed"`
}

// LatencyStats summarizes one location over the whole range. Avg, Min and
// Max cover the successful checks only.
type LatencyStats struct {
	Avg    *float64 `json:"avg"`
	Min    *float64 `json:"min"`
	Max    *float64 `json:"max"`
	Checks int      `json:"checks"`
	Failed int      `json:"failed"`
}

// Latency holds the plotted series and the stats per location. Bucket is the
// width in seconds of each grouped point, or 0 when no series was grouped.
type Latency struct {
	From   int64                   `json:"from"`
	To     int64                   `json:"to"`
	Bucket int64                   `json:"bucket"`
	Series map[string][]Point      `json:"series"`
	Stats  map[string]LatencyStats `json:"stats"`
}

type entry struct {
	t        int64
	span     int64
	checks   int
	failed   int
	okCount  int
	sum      float64
	min, max float64
}

func (e *entry) merge(o entry) {
	if o.okCount > 0 {
		if e.okCount == 0 || o.min < e.min {
			e.min = o.min
		}
		if e.okCount == 0 || o.max > e.max {
			e.max = o.max
		}
	}
	e.checks += o.checks
	e.failed += o.failed
	e.okCount += o.okCount
	e.sum += o.sum
}

func (e entry) point(t, span int64) Point {
	p := Point{T: t, Span: span, Checks: e.checks, Failed: e.failed}
	if e.okCount > 0 {
		avg, lo, hi := e.sum/float64(e.okCount), e.min, e.max
		p.Avg, p.Min, p.Max = &avg, &lo, &hi
	}
	return p
}

// rawHour is one location's raw checks for one hour: those inside the range,
// and how many the hour holds in all.
type rawHour struct {
	entries []entry
	total   int
}

func rollupEntry(hour int64, lr LocationRollup) entry {
	e := entry{t: hour, span: 3600, checks: lr.Total, failed: lr.Failure}
	measured := lr.Measured
	if measured == 0 && lr.AvgMs > 0 {
		measured = lr.Success
	}
	if measured > 0 {
		e.okCount = measured
		e.sum = lr.AvgMs * float64(measured)
		e.min, e.max = lr.MinMs, lr.MaxMs
		if lr.MaxMs == 0 {
			e.min, e.max = lr.AvgMs, lr.AvgMs
		}
	}
	return e
}

// Latency returns each location's checks in [from, to] for plotting.
//
// A finished hour that has a rollup for the location comes from the rollup
// when the rollup counts more checks than the raw samples still hold, which
// covers hours whose raw samples have expired in full or in part. The
// current and previous hours, which the rollup pass may not have caught up
// with yet, come from the raw samples when there are any. An hour starting
// before from is left to its raw checks, since its rollup cannot be split.
//
// When maxPoints is positive and any location has more entries than that,
// every series is grouped into equal time buckets, at most maxPoints of them.
func (s *Store) Latency(ctx context.Context, dash, svc string, from, to time.Time, maxPoints int) (Latency, error) {
	rollups, err := s.GetAggregateRollups(ctx, dash, svc, from.Truncate(time.Hour), to)
	if err != nil {
		return Latency{}, err
	}
	settled := to.Truncate(time.Hour).Add(-time.Hour).Unix()
	out := Latency{From: from.Unix(), To: to.Unix(), Series: map[string][]Point{}, Stats: map[string]LatencyStats{}}
	byLocation := map[string][]entry{}
	for _, name := range s.LocationNames() {
		raw, err := s.rawEntries(ctx, s.locations[name], dash, svc, from, to)
		if err != nil {
			log.Printf("store: reading samples of %s/%s at %s: %v", dash, svc, name, err)
			raw = nil
		}
		var entries []entry
		for hour, rh := range raw {
			if _, ok := rollups[hour].PerLocation[name]; !ok {
				entries = append(entries, rh.entries...)
			}
		}
		for hour, r := range rollups {
			lr, ok := r.PerLocation[name]
			if !ok {
				continue
			}
			rh := raw[hour]
			useRollup := hour >= from.Unix() && (rh == nil || (hour < settled && lr.Total > rh.total))
			if useRollup {
				entries = append(entries, rollupEntry(hour, lr))
			} else if rh != nil {
				entries = append(entries, rh.entries...)
			}
		}
		if len(entries) > 0 {
			sort.Slice(entries, func(i, j int) bool { return entries[i].t < entries[j].t })
			byLocation[name] = entries
		}
	}

	span := out.To - out.From
	for _, entries := range byLocation {
		if maxPoints > 0 && len(entries) > maxPoints {
			out.Bucket = span/int64(maxPoints) + 1
		}
	}
	for name, entries := range byLocation {
		var total entry
		var points []Point
		if out.Bucket == 0 {
			points = make([]Point, 0, len(entries))
			for _, e := range entries {
				points = append(points, e.point(e.t, e.span))
				total.merge(e)
			}
		} else {
			var cur entry
			var curIdx int64
			started := false
			for _, e := range entries {
				idx := (e.t - out.From) / out.Bucket
				if started && idx != curIdx {
					points = append(points, cur.point(out.From+curIdx*out.Bucket, out.Bucket))
					cur = entry{}
				}
				curIdx, started = idx, true
				cur.merge(e)
				total.merge(e)
			}
			if started {
				points = append(points, cur.point(out.From+curIdx*out.Bucket, out.Bucket))
			}
		}
		out.Series[name] = points
		p := total.point(0, 0)
		out.Stats[name] = LatencyStats{Avg: p.Avg, Min: p.Min, Max: p.Max, Checks: p.Checks, Failed: p.Failed}
	}
	return out, nil
}

// rawEntries reads one location's raw checks hour by hour over [from, to],
// keeping one entry per check inside the range.
func (s *Store) rawEntries(ctx context.Context, c *Client, dash, svc string, from, to time.Time) (map[int64]*rawHour, error) {
	var hours []int64
	var cmds []valkey.Completed
	for h := from.Truncate(time.Hour); !h.After(to); h = h.Add(time.Hour) {
		hours = append(hours, h.Unix())
		cmds = append(cmds,
			c.client.B().Hgetall().Key(keys.StatusHour(dash, svc, h.Unix())).Build(),
			c.client.B().Hgetall().Key(keys.DetailHour(dash, svc, h.Unix())).Build())
	}
	if len(cmds) == 0 {
		return nil, nil
	}
	results := c.client.DoMulti(ctx, cmds...)
	out := map[int64]*rawHour{}
	for i, hour := range hours {
		statusMap, err := results[2*i].AsStrMap()
		if err != nil {
			return nil, err
		}
		detailMap, err := results[2*i+1].AsStrMap()
		if err != nil {
			return nil, err
		}
		if len(statusMap) == 0 {
			continue
		}
		rh := &rawHour{total: len(statusMap)}
		for tss, st := range statusMap {
			ts, err := strconv.ParseInt(tss, 10, 64)
			if err != nil || ts < from.Unix() || ts > to.Unix() {
				continue
			}
			e := entry{t: ts, checks: 1}
			if st == "f" {
				e.failed = 1
			} else if d, ok := detailMap[tss]; ok {
				ms, _ := parseDetail(d)
				e.okCount, e.sum, e.min, e.max = 1, float64(ms), float64(ms), float64(ms)
			}
			rh.entries = append(rh.entries, e)
		}
		out[hour] = rh
	}
	return out, nil
}
