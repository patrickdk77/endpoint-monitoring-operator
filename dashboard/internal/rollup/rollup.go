package rollup

import (
	"context"
	"log"
	"time"

	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/store"
)

type Runner struct {
	store    *store.Store
	interval time.Duration
	pace     time.Duration
	hours    int
}

func New(s *store.Store, interval, pace time.Duration, retentionDays int) *Runner {
	return &Runner{store: s, interval: interval, pace: pace, hours: retentionDays * 24}
}

func (r *Runner) Start(ctx context.Context) {
	// Fill any historical gaps in the background so the live periodic rollup
	// below is never blocked waiting on a long backfill.
	go r.backfill(ctx, time.Now().UTC())

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
	touched  map[int64]struct{}
}

// backfill scans the full retention window and computes any hour that is
// missing an aggregate rollup in the primary store. It steps hour-by-hour
// across every monitor (the most recent completed hour first, then one hour
// further back, and so on) so recent history fills in across all monitors
// before older history. It paces itself by sleeping between each gap hour it
// processes so it does not hammer the source locations on startup.
func (r *Runner) backfill(ctx context.Context, at time.Time) {
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
			rollups, err := r.store.GetAggregateRollups(ctx, dash, svc, oldest, now)
			if err != nil {
				log.Printf("rollup: backfill read rollups %s/%s: %v", dash, svc, err)
				continue
			}
			existing := make(map[int64]struct{}, len(rollups))
			for epoch := range rollups {
				existing[epoch] = struct{}{}
			}
			refs = append(refs, svcRef{dash: dash, svc: svc, existing: existing, touched: map[int64]struct{}{}})
		}
	}

	filled := 0
	// Offset 0 is the most recent completed hour (now-1h); step back from there.
	for offset := 1; offset <= r.hours; offset++ {
		hour := now.Add(-time.Duration(offset) * time.Hour)
		for _, ref := range refs {
			if ctx.Err() != nil {
				return
			}
			if _, ok := ref.existing[hour.Unix()]; ok {
				continue
			}
			if r.rollupServiceHour(ctx, ref.dash, ref.svc, hour) {
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
	// wrote, and of every finished day fully inside the window that has none.
	today := at.Truncate(24 * time.Hour)
	firstFull := oldest.Truncate(24 * time.Hour)
	if firstFull.Before(oldest) {
		firstFull = firstFull.AddDate(0, 0, 1)
	}
	days := 0
	for _, ref := range refs {
		dailies, err := r.store.GetDailyRollups(ctx, ref.dash, ref.svc, firstFull, today.AddDate(0, 0, -1))
		if err != nil {
			log.Printf("rollup: backfill read daily rollups %s/%s: %v", ref.dash, ref.svc, err)
			continue
		}
		for day := firstFull; day.Before(today); day = day.AddDate(0, 0, 1) {
			_, touched := ref.touched[day.Unix()]
			if _, ok := dailies[day.Format("2006-01-02")]; ok && !touched {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			r.updateDaily(ctx, ref.dash, ref.svc, day)
			days++
			select {
			case <-ctx.Done():
				return
			case <-time.After(r.pace):
			}
		}
	}
	log.Printf("rollup: backfill complete, filled %d gap hour(s), rebuilt %d daily rollup(s)", filled, days)
}

func (r *Runner) runOnce(ctx context.Context, now time.Time) {
	dashboards, err := r.store.ListDashboards(ctx)
	if err != nil {
		log.Printf("rollup: list dashboards: %v", err)
		return
	}

	today := now.Truncate(24 * time.Hour)
	hours := []time.Time{
		now.Truncate(time.Hour),
		now.Truncate(time.Hour).Add(-time.Hour),
	}

	for _, dash := range dashboards {
		services, err := r.store.ListServices(ctx, dash)
		if err != nil {
			log.Printf("rollup: list services for %s: %v", dash, err)
			continue
		}
		for _, svc := range services {
			for _, hour := range hours {
				r.rollupServiceHour(ctx, dash, svc, hour)
				if hour.Before(today) {
					r.updateDaily(ctx, dash, svc, hour)
				}
			}
		}
	}
}

func (r *Runner) updateDaily(ctx context.Context, dash, svc string, day time.Time) {
	if err := r.store.UpdateDailyRollup(ctx, dash, svc, day); err != nil {
		log.Printf("rollup: update daily %s/%s %s: %v", dash, svc, day.Format("2006-01-02"), err)
	}
}

// rollupServiceHour computes and persists the rollups for a single service-hour.
// It returns true when an aggregate rollup was written (i.e. data existed).
func (r *Runner) rollupServiceHour(ctx context.Context, dash, svc string, hour time.Time) bool {
	aggregate, ok := r.store.ComputeAggregate(ctx, dash, svc, hour)
	if !ok {
		return false
	}

	if err := r.store.WriteAggregateRollup(ctx, dash, svc, hour, aggregate); err != nil {
		log.Printf("rollup: write aggregate rollup %s %s: %v", dash, svc, err)
		return false
	}
	return true
}
