package rollup

import (
	"context"
	"log"
	"math"
	"sync"
	"time"

	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/store"
)

type Runner struct {
	store    *store.Store
	interval time.Duration
	pace     time.Duration
	hours    int
	spawn    func(func())

	mu          sync.Mutex
	backfilling bool
	down        map[string]error
	readMissing bool
	recovered   bool
	storeFailed bool
	needFull    bool
	lastFull    time.Time
}

// fullBackfillGap is the least time between two full backfills started after
// a failure on the primary.
const fullBackfillGap = time.Hour

func New(s *store.Store, interval, pace time.Duration, retentionDays int) *Runner {
	return &Runner{store: s, interval: interval, pace: pace, hours: retentionDays * 24, spawn: func(f func()) { go f() }}
}

func (r *Runner) Start(ctx context.Context) {
	// Fill any historical gaps in the background so the live periodic rollup
	// below is never blocked waiting on a long backfill.
	start := time.Now().UTC()
	r.mu.Lock()
	r.backfilling, r.lastFull = true, start
	r.mu.Unlock()
	r.launchBackfill(ctx, start, true)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	r.runOnce(ctx, time.Now().UTC())
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.runOnce(ctx, time.Now().UTC())
		}
	}
}

// svcRef identifies a single monitor (service) within a dashboard, along with
// the set of hours that already have an aggregate rollup in the primary store.
type svcRef struct {
	dash     string
	svc      string
	existing map[int64]struct{}
	marked   map[int64]struct{}
	touched  map[int64]struct{}
	first    int64
}

// launchBackfill runs backfill through spawn. The caller sets backfilling.
func (r *Runner) launchBackfill(ctx context.Context, at time.Time, full bool) {
	r.spawn(func() {
		r.backfill(ctx, at, full)
		r.mu.Lock()
		r.backfilling = false
		r.mu.Unlock()
	})
}

// backfill scans the full retention window and computes every hour whose
// rollup was stored without a location that could not be read or before the
// hour ended. A full backfill also computes every hour missing an aggregate
// rollup in the primary store, and gives every stored rollup field its TTL.
// It steps hour-by-hour across every monitor (the most recent completed hour
// first, then one hour further back, and so on) so recent history fills in
// across all monitors before older history. It paces itself by sleeping
// between each gap hour it processes so it does not hammer the source
// locations on startup.
func (r *Runner) backfill(ctx context.Context, at time.Time, full bool) {
	dashboards, err := r.store.ListDashboards(ctx)
	if err != nil {
		log.Printf("rollup: backfill list dashboards: %v", err)
		return
	}

	now := at.Truncate(time.Hour)

	var refs []svcRef
	oldest := now.Add(-time.Duration(r.hours-1) * time.Hour)
	for _, dash := range dashboards {
		services, err := r.store.ListServices(ctx, dash)
		if err != nil {
			log.Printf("rollup: backfill list services for %s: %v", dash, err)
			continue
		}
		for _, svc := range services {
			if full {
				if err := r.store.ExpireRollupFields(ctx, dash, svc, at); err != nil {
					log.Printf("rollup: backfill expire rollup fields %s/%s: %v", dash, svc, err)
				}
			}
			rollups, err := r.store.GetAggregateRollups(ctx, dash, svc, oldest, now)
			if err != nil {
				log.Printf("rollup: backfill read rollups %s/%s: %v", dash, svc, err)
				continue
			}
			ref := svcRef{dash: dash, svc: svc, existing: map[int64]struct{}{}, marked: map[int64]struct{}{}, touched: map[int64]struct{}{}, first: math.MaxInt64}
			for epoch, rollup := range rollups {
				ref.first = min(ref.first, epoch)
				if len(rollup.Missing) == 0 && !rollup.InProgress {
					ref.existing[epoch] = struct{}{}
				} else {
					ref.marked[epoch] = struct{}{}
				}
			}
			refs = append(refs, ref)
		}
	}

	filled := 0
	// Offset 0 is the most recent completed hour (now-1h); step back from there.
	for offset := 1; offset < r.hours; offset++ {
		hour := now.Add(-time.Duration(offset) * time.Hour)
		for _, ref := range refs {
			if ctx.Err() != nil {
				return
			}
			_, done := ref.existing[hour.Unix()]
			_, marked := ref.marked[hour.Unix()]
			if done || (!full && !marked) {
				continue
			}
			if res, _ := r.rollupServiceHour(ctx, ref.dash, ref.svc, hour, at, marked || hour.Unix() >= ref.first); res.Written {
				filled++
				ref.touched[hour.Truncate(24*time.Hour).Unix()] = struct{}{}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(r.pace):
			}
		}
	}

	// Rebuild the daily rollup of every finished day whose hours backfill
	// wrote, and in a full backfill of every finished day fully inside the
	// window that has none.
	today := at.Truncate(24 * time.Hour)
	firstFull := oldest.Truncate(24 * time.Hour)
	if firstFull.Before(oldest) {
		firstFull = firstFull.AddDate(0, 0, 1)
	}
	days := 0
	for _, ref := range refs {
		var dailies map[string]store.DailyRollup
		if full {
			dailies, err = r.store.GetDailyRollups(ctx, ref.dash, ref.svc, firstFull, today.AddDate(0, 0, -1))
			if err != nil {
				log.Printf("rollup: backfill read daily rollups %s/%s: %v", ref.dash, ref.svc, err)
				continue
			}
		}
		for day := firstFull; day.Before(today); day = day.AddDate(0, 0, 1) {
			_, touched := ref.touched[day.Unix()]
			_, has := dailies[day.Format("2006-01-02")]
			if !touched && (has || !full) {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			if written, _ := r.updateDaily(ctx, ref.dash, ref.svc, day, at); written {
				days++
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(r.pace):
			}
		}
	}
	if full || filled > 0 || days > 0 {
		log.Printf("rollup: backfill complete, filled %d gap hour(s), rebuilt %d daily rollup(s)", filled, days)
	}
}

func (r *Runner) runOnce(ctx context.Context, now time.Time) {
	dashboards, err := r.store.ListDashboards(ctx)
	if err != nil {
		log.Printf("rollup: list dashboards: %v", err)
		r.noteStoreFailed()
		return
	}

	today := now.Truncate(24 * time.Hour)
	hours := []time.Time{
		now.Truncate(time.Hour),
		now.Truncate(time.Hour).Add(-time.Hour),
	}

	down := map[string]error{}
	rolled, failed := false, false
	for _, dash := range dashboards {
		services, err := r.store.ListServices(ctx, dash)
		if err != nil {
			log.Printf("rollup: list services for %s: %v", dash, err)
			r.noteStoreFailed()
			failed = true
			continue
		}
		for _, svc := range services {
			for _, hour := range hours {
				res, err := r.rollupServiceHour(ctx, dash, svc, hour, now, true)
				rolled = true
				failed = failed || err != nil
				for loc, lerr := range res.Missing {
					down[loc] = lerr
				}
				if hour.Before(today) {
					_, err := r.updateDaily(ctx, dash, svc, hour, now)
					failed = failed || err != nil
				}
			}
		}
	}
	if rolled {
		r.afterPass(ctx, now, down, failed)
	}
}

func (r *Runner) noteStoreFailed() {
	r.mu.Lock()
	r.storeFailed, r.needFull = true, true
	r.mu.Unlock()
}

// afterPass logs the locations that stopped or started answering, and starts
// a backfill of the marked hours to finish what earlier passes and backfills
// left incomplete: once a location that could not be read answers again, or
// every location answers, and once a pass stores everything after a failure
// to list, read or write on the primary. That backfill is a full one when
// the primary failed since the last full backfill and that one started at
// least fullBackfillGap ago.
func (r *Runner) afterPass(ctx context.Context, now time.Time, down map[string]error, failed bool) {
	r.mu.Lock()
	for loc := range r.down {
		if _, still := down[loc]; !still {
			log.Printf("rollup: location %s answers again", loc)
			r.recovered = true
		}
	}
	for loc, err := range down {
		if _, was := r.down[loc]; !was {
			log.Printf("rollup: location %s cannot be read; storing hours without it until it answers: %v", loc, err)
		}
	}
	r.down = down
	full := r.needFull && !failed && now.Sub(r.lastFull) >= fullBackfillGap
	repair := (r.readMissing && (r.recovered || len(down) == 0)) || (r.storeFailed && !failed)
	start := !r.backfilling && (full || repair)
	if start {
		r.backfilling, r.readMissing, r.recovered, r.storeFailed = true, false, false, false
		if full {
			r.needFull, r.lastFull = false, now
		}
	}
	r.mu.Unlock()
	if start {
		r.launchBackfill(ctx, now, full)
	}
}

func (r *Runner) updateDaily(ctx context.Context, dash, svc string, day, now time.Time) (bool, error) {
	written, err := r.store.UpdateDailyRollup(ctx, dash, svc, day, now)
	if err != nil {
		log.Printf("rollup: update daily %s/%s %s: %v", dash, svc, day.Format("2006-01-02"), err)
		r.noteStoreFailed()
	}
	return written, err
}

// rollupServiceHour stores the rollups for a single service-hour and notes a
// location it could not read or a failure on the primary, so a later backfill
// finishes the hour. mark is passed on to RollupHour.
func (r *Runner) rollupServiceHour(ctx context.Context, dash, svc string, hour, now time.Time, mark bool) (store.HourRollup, error) {
	res, err := r.store.RollupHour(ctx, dash, svc, hour, now, mark)
	if err != nil {
		log.Printf("rollup: store %s/%s %s: %v", dash, svc, hour.Format(time.RFC3339), err)
	}
	if len(res.Missing) > 0 || err != nil {
		r.mu.Lock()
		r.readMissing = r.readMissing || len(res.Missing) > 0
		r.storeFailed = r.storeFailed || err != nil
		r.needFull = r.needFull || err != nil
		r.mu.Unlock()
	}
	return res, err
}
