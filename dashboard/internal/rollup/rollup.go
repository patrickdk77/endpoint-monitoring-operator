package rollup

import (
	"context"
	"log"
	"time"

	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/status"
	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/store"
)

type Runner struct {
	store    *store.Store
	interval time.Duration
}

func New(s *store.Store, interval time.Duration) *Runner {
	return &Runner{store: s, interval: interval}
}

func (r *Runner) Start(ctx context.Context) {
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

func (r *Runner) rollupServiceHour(ctx context.Context, dash, svc string, hour time.Time) {
	perLocation := make(map[string]store.LocationRollup)
	var locStatuses []status.LocationStatus
	var totalSuccess, totalFailure int
	var totalMs float64
	var msSamples int

	for _, loc := range r.store.LocationNames() {
		rollup, err := r.store.ComputeLocationRollup(ctx, loc, dash, svc, hour)
		if err != nil {
			log.Printf("rollup: %s %s %s %s: %v", loc, dash, svc, hour.Format(time.RFC3339), err)
			continue
		}
		if rollup.Total == 0 {
			continue
		}
		if err := r.store.WriteLocationRollup(ctx, dash, svc, loc, hour, rollup); err != nil {
			log.Printf("rollup: write location rollup: %v", err)
		}
		perLocation[loc] = rollup
		locStatuses = append(locStatuses, status.LocationStatus(rollup.Status))
		totalSuccess += rollup.Success
		totalFailure += rollup.Failure
		if rollup.AvgMs > 0 {
			totalMs += rollup.AvgMs
			msSamples++
		}
	}

	aggStatus := status.AggregateFromLocations(locStatuses)
	aggregate := store.AggregateRollup{
		Status:      string(aggStatus),
		Success:     totalSuccess,
		Failure:     totalFailure,
		PerLocation: perLocation,
	}
	if msSamples > 0 {
		aggregate.AvgMs = totalMs / float64(msSamples)
	}

	if len(locStatuses) == 0 {
		return
	}
	if err := r.store.WriteAggregateRollup(ctx, dash, svc, hour, aggregate); err != nil {
		log.Printf("rollup: write aggregate rollup: %v", err)
	}
}
