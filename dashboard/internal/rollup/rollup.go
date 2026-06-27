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
	go r.backfill(ctx)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	r.runOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.runOnce(ctx)
		}
	}
}

// svcRef identifies a single monitor (service) within a dashboard, along with
// the set of hours that already have an aggregate rollup in the primary store.
type svcRef struct {
	dash     string
	svc      string
	existing map[int64]struct{}
}

// backfill scans the full retention window and computes any hour that is
// missing an aggregate rollup in the primary store. It steps hour-by-hour
// across every monitor (the most recent completed hour first, then one hour
// further back, and so on) so recent history fills in across all monitors
// before older history. It paces itself by sleeping between each gap hour it
// processes so it does not hammer the source locations on startup.
func (r *Runner) backfill(ctx context.Context) {
	dashboards, err := r.store.ListDashboards(ctx)
	if err != nil {
		log.Printf("rollup: backfill list dashboards: %v", err)
		return
	}

	now := time.Now().UTC().Truncate(time.Hour)

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
			refs = append(refs, svcRef{dash: dash, svc: svc, existing: existing})
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
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(r.pace):
			}
		}
	}
	log.Printf("rollup: backfill complete, filled %d gap hour(s)", filled)
}

func (r *Runner) runOnce(ctx context.Context) {
	dashboards, err := r.store.ListDashboards(ctx)
	if err != nil {
		log.Printf("rollup: list dashboards: %v", err)
		return
	}

	now := time.Now().UTC()
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
			}
		}
	}
}

// rollupServiceHour computes and persists the rollups for a single service-hour.
// It returns true when an aggregate rollup was written (i.e. data existed).
func (r *Runner) rollupServiceHour(ctx context.Context, dash, svc string, hour time.Time) bool {
	aggregate, ok := r.store.ComputeAggregate(ctx, dash, svc, hour)
	if !ok {
		return false
	}

	for loc, lr := range aggregate.PerLocation {
		if err := r.store.WriteLocationRollup(ctx, dash, svc, loc, hour, lr); err != nil {
			log.Printf("rollup: write location rollup %s %s %s: %v", loc, dash, svc, err)
		}
	}

	if err := r.store.WriteAggregateRollup(ctx, dash, svc, hour, aggregate); err != nil {
		log.Printf("rollup: write aggregate rollup %s %s: %v", dash, svc, err)
		return false
	}
	return true
}
