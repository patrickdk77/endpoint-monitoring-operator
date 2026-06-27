package server

import (
	"context"
	"embed"
	"encoding/json"
	"html/template"
	"io/fs"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/status"
	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/store"
)

//go:embed templates/*
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

type Server struct {
	store     *store.Store
	retention int
	templates *template.Template
}

func New(s *store.Store, retentionDays int) (*Server, error) {
	tmpl, err := template.ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{store: s, retention: retentionDays, templates: tmpl}, nil
}

func (s *Server) Handler() http.Handler {
	static, _ := fs.Sub(staticFS, "static")
	staticHandler := http.StripPrefix("/static/", http.FileServer(http.FS(static)))

	apiMux := http.NewServeMux()
	apiMux.HandleFunc("GET /dashboards", s.handleAPIDashboards)
	apiMux.HandleFunc("GET /dashboard/{dash}/overview", s.handleAPIOverview)
	apiMux.HandleFunc("GET /dashboard/{dash}/{name}", s.handleAPIService)

	pageMux := http.NewServeMux()
	pageMux.HandleFunc("GET /{dash}", s.handleDashboard)
	pageMux.HandleFunc("GET /{dash}/{name}", s.handleService)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/static/"):
			staticHandler.ServeHTTP(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/"):
			http.StripPrefix("/api", apiMux).ServeHTTP(w, r)
		default:
			pageMux.ServeHTTP(w, r)
		}
	})
}

type serviceOverview struct {
	ID     string
	Meta   store.Meta
	Status string
	Daily  []dayBar
}

type dayBar struct {
	Date   string
	Status string
	Label  string
}

type dashboardData struct {
	Name     string
	Services []serviceOverview
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	dash := r.PathValue("dash")
	services, _ := s.store.ListServices(r.Context(), dash)
	sort.Strings(services)

	var items []serviceOverview
	for _, svc := range services {
		meta, _ := s.store.GetMeta(r.Context(), dash, svc)
		daily := s.dailyBars(r.Context(), dash, svc)
		current := status.AggregateOperational
		if len(daily) > 0 {
			current = status.AggregateStatus(daily[len(daily)-1].Status)
		}
		items = append(items, serviceOverview{
			ID:     svc,
			Meta:   meta,
			Status: string(current),
			Daily:  daily,
		})
	}

	s.render(w, "dashboard.html", dashboardData{Name: dash, Services: items})
}

func (s *Server) dailyBars(ctx context.Context, dash, svc string) []dayBar {
	now := time.Now().UTC()
	from := now.AddDate(0, 0, -s.retention).Truncate(24 * time.Hour)
	to := now
	rollups, _ := s.store.GetAggregateRollups(ctx, dash, svc, from, to)

	byDay := map[string]status.AggregateStatus{}
	for hourEpoch, rollup := range rollups {
		day := time.Unix(hourEpoch, 0).UTC().Format("2006-01-02")
		st := status.AggregateStatus(rollup.Status)
		byDay[day] = status.WorstAggregate(byDay[day], st)
	}

	var bars []dayBar
	for d := 0; d < s.retention; d++ {
		day := from.AddDate(0, 0, d)
		key := day.Format("2006-01-02")
		st := byDay[key]
		if st == "" {
			st = status.AggregateUnknown
		}
		bars = append(bars, dayBar{
			Date:   key,
			Status: string(st),
			Label:  key,
		})
	}
	return bars
}

type hourBar struct {
	Hour   string
	Status string
	Detail store.AggregateRollup
}

type serviceData struct {
	Dashboard string
	ServiceID string
	Meta      store.Meta
	Hours     []hourBar
	Daily     []dayBar
}

func (s *Server) handleService(w http.ResponseWriter, r *http.Request) {
	dash := r.PathValue("dash")
	svc := r.PathValue("name")

	meta, _ := s.store.GetMeta(r.Context(), dash, svc)
	now := time.Now().UTC()
	from := now.Add(-24 * time.Hour).Truncate(time.Hour)
	rollups, _ := s.store.GetAggregateRollups(r.Context(), dash, svc, from, now)

	var hours []hourBar
	for h := from; !h.After(now); h = h.Add(time.Hour) {
		rollup, ok := rollups[h.Unix()]
		st := string(status.AggregateUnknown)
		if ok {
			st = rollup.Status
		}
		hours = append(hours, hourBar{
			Hour:   h.Format("15:04"),
			Status: st,
			Detail: rollup,
		})
	}

	s.render(w, "service.html", serviceData{
		Dashboard: dash,
		ServiceID: svc,
		Meta:      meta,
		Hours:     hours,
		Daily:     s.dailyBars(r.Context(), dash, svc),
	})
}

func (s *Server) handleAPIDashboards(w http.ResponseWriter, r *http.Request) {
	dashboards, err := s.store.ListDashboards(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, dashboards)
}

func (s *Server) handleAPIOverview(w http.ResponseWriter, r *http.Request) {
	dash := r.PathValue("dash")
	services, err := s.store.ListServices(r.Context(), dash)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type item struct {
		Service string   `json:"service"`
		Meta    store.Meta `json:"meta"`
		Daily   []dayBar `json:"daily"`
	}
	var out []item
	for _, svc := range services {
		meta, _ := s.store.GetMeta(r.Context(), dash, svc)
		out = append(out, item{Service: svc, Meta: meta, Daily: s.dailyBars(r.Context(), dash, svc)})
	}
	writeJSON(w, out)
}

func (s *Server) handleAPIService(w http.ResponseWriter, r *http.Request) {
	dash := r.PathValue("dash")
	svc := r.PathValue("name")
	now := time.Now().UTC()
	from := now.AddDate(0, 0, -s.retention).Truncate(24 * time.Hour)
	rollups, err := s.store.GetAggregateRollups(r.Context(), dash, svc, from, now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	meta, _ := s.store.GetMeta(r.Context(), dash, svc)
	writeJSON(w, map[string]any{
		"service": svc,
		"meta":    meta,
		"rollups": rollups,
		"daily":   s.dailyBars(r.Context(), dash, svc),
	})
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
