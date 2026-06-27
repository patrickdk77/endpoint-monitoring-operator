package server

import (
	"compress/flate"
	"compress/gzip"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"html"
	"html/template"
	"io"
	"io/fs"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	staticFSRoot, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}

	hashes, err := computeStaticHashes(staticFSRoot)
	if err != nil {
		return nil, err
	}

	tmpl := template.New("").Funcs(template.FuncMap{
		"formatSLA": func(s string) template.HTML {
			if s == "" {
				return template.HTML("&mdash;")
			}
			if strings.HasSuffix(s, "%") {
				num := strings.TrimSuffix(s, "%")
				class := "sla-good"
				if n, err := strconv.ParseFloat(num, 64); err == nil {
					switch {
					case n >= 99.9:
						class = "sla-good"
					case n >= 99.5:
						class = "sla-warning"
					default:
						class = "sla-danger"
					}
				}
				return template.HTML(fmt.Sprintf(`<span class="%s"><span class="sla-num">%s</span><span class="sla-percent">%%</span></span>`, class, html.EscapeString(num)))
			}
			return template.HTML(html.EscapeString(s))
		},
		"assetURL": func(name string) string {
			return assetURL(name, hashes)
		},
	})
	var err2 error
	tmpl, err2 = tmpl.ParseFS(templateFS, "templates/*.html")
	if err2 != nil {
		return nil, err2
	}
	return &Server{store: s, retention: retentionDays, templates: tmpl}, nil
}

func computeStaticHashes(fsys fs.FS) (map[string]string, error) {
	hashes := map[string]string{}
	if err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		hashes[path] = fmt.Sprintf("%08x", crc32.ChecksumIEEE(b))
		return nil
	}); err != nil {
		return nil, err
	}
	return hashes, nil
}

func assetURL(name string, hashes map[string]string) string {
	if hash, ok := hashes[name]; ok {
		return "/static/" + name + "?v=" + hash
	}
	return "/static/" + name
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

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/static/"):
			staticHandler.ServeHTTP(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/"):
			http.StripPrefix("/api", apiMux).ServeHTTP(w, r)
		default:
			pageMux.ServeHTTP(w, r)
		}
	})

	return NoCache(Compress(handler))
}

type compressResponseWriter struct {
	http.ResponseWriter
	compressor  io.WriteCloser
	wroteHeader bool
	encoding    string
}

var (
	gzipPool = sync.Pool{
		New: func() interface{} {
			return gzip.NewWriter(io.Discard)
		},
	}
	flatePool = sync.Pool{
		New: func() interface{} {
			w, _ := flate.NewWriter(io.Discard, flate.DefaultCompression)
			return w
		},
	}
)

func isCompressible(contentType string) bool {
	contentType = strings.Split(contentType, ";")[0]
	contentType = strings.TrimSpace(contentType)
	switch contentType {
	case "text/html", "text/css", "text/plain", "text/xml", "text/javascript":
		return true
	case "application/javascript", "application/x-javascript", "application/json", "application/xml":
		return true
	case "image/svg+xml":
		return true
	}
	return false
}

func (w *compressResponseWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true

	contentType := w.Header().Get("Content-Type")

	if code == http.StatusNoContent || code == http.StatusNotModified ||
		w.Header().Get("Content-Encoding") != "" || !isCompressible(contentType) {
		w.encoding = ""
		w.ResponseWriter.WriteHeader(code)
		return
	}

	w.Header().Set("Content-Encoding", w.encoding)
	w.Header().Add("Vary", "Accept-Encoding")
	w.Header().Del("Content-Length")

	if w.encoding == "gzip" {
		enc := gzipPool.Get().(*gzip.Writer)
		enc.Reset(w.ResponseWriter)
		w.compressor = enc
	} else if w.encoding == "deflate" {
		enc := flatePool.Get().(*flate.Writer)
		enc.Reset(w.ResponseWriter)
		w.compressor = enc
	}

	w.ResponseWriter.WriteHeader(code)
}

func (w *compressResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", http.DetectContentType(b))
		}
		w.WriteHeader(http.StatusOK)
	}
	if w.encoding != "" && w.compressor != nil {
		return w.compressor.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

func (w *compressResponseWriter) Flush() {
	if w.compressor != nil {
		if flusher, ok := w.compressor.(interface{ Flush() error }); ok {
			flusher.Flush()
		}
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func Compress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Connection"), "Upgrade") {
			next.ServeHTTP(w, r)
			return
		}

		accept := r.Header.Get("Accept-Encoding")
		encoding := ""
		if strings.Contains(accept, "gzip") {
			encoding = "gzip"
		} else if strings.Contains(accept, "deflate") {
			encoding = "deflate"
		}

		if encoding == "" {
			next.ServeHTTP(w, r)
			return
		}

		cw := &compressResponseWriter{
			ResponseWriter: w,
			encoding:       encoding,
		}

		next.ServeHTTP(cw, r)

		if cw.encoding != "" && cw.compressor != nil {
			cw.compressor.Close()
			if cw.encoding == "gzip" {
				gzipPool.Put(cw.compressor)
			} else if cw.encoding == "deflate" {
				flatePool.Put(cw.compressor)
			}
		}
	})
}

func NoCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/.well-known/") || strings.HasPrefix(r.URL.Path, "/static/") || strings.HasPrefix(r.URL.Path, "/robots.txt") {
			w.Header().Set("Cache-Control", "public, max-age=2600000")
		} else {
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		}
		next.ServeHTTP(w, r)
	})
}

type serviceOverview struct {
	ID      string
	Display string
	Meta    store.Meta
	Status  string
	SLA     string
	Daily   []dayBar
}

type dayBar struct {
	Date   string
	Status string
	Label  string
}

type serviceGroup struct {
	Name     string
	Services []serviceOverview
}

type dashboardData struct {
	Name   string
	Groups []serviceGroup
}

// splitGroup splits a monitor name of the form "group_name" into its group and
// display name. Only a single underscore triggers grouping; names with no
// underscore or with multiple underscores are treated as ungrouped and keep
// their full name.
func splitGroup(id string) (group, display string) {
	i := strings.IndexByte(id, '_')
	if i <= 0 {
		return "", id
	}
	rest := id[i+1:]
	if rest == "" || strings.Contains(rest, "_") {
		return "", id
	}
	return id[:i], rest
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	dash := r.PathValue("dash")
	services, _ := s.store.ListServices(r.Context(), dash)
	sort.Strings(services)

	groupIndex := map[string]int{}
	var groups []serviceGroup
	for _, svc := range services {
		meta, _ := s.store.GetMeta(r.Context(), dash, svc)
		daily := s.dailyBars(r.Context(), dash, svc)
		current := status.AggregateOperational
		if len(daily) > 0 {
			current = status.AggregateStatus(daily[len(daily)-1].Status)
		}
		name := svc
		if meta.Name != "" {
			name = meta.Name
		}
		group, display := splitGroup(name)
		ov := serviceOverview{
			ID:      svc,
			Display: display,
			Meta:    meta,
			Status:  string(current),
			SLA:     s.sla(r.Context(), dash, svc),
			Daily:   daily,
		}
		idx, ok := groupIndex[group]
		if !ok {
			idx = len(groups)
			groupIndex[group] = idx
			groups = append(groups, serviceGroup{Name: group})
		}
		groups[idx].Services = append(groups[idx].Services, ov)
	}

	s.render(w, "dashboard.html", dashboardData{Name: dash, Groups: groups})
}

func (s *Server) dailyBars(ctx context.Context, dash, svc string) []dayBar {
	now := time.Now().UTC()
	// Render retention days ending with today (inclusive).
	from := now.AddDate(0, 0, -(s.retention - 1)).Truncate(24 * time.Hour)
	rollups, _ := s.store.GetAggregateRollups(ctx, dash, svc, from, now)

	// Overlay the current hour computed live, so today's value reflects state
	// changes immediately rather than waiting for the next rollup pass.
	curHour := now.Truncate(time.Hour)
	if agg, ok := s.store.ComputeAggregate(ctx, dash, svc, curHour); ok {
		rollups[curHour.Unix()] = agg
	}

	// Hours with no data are never stored or overlaid, so unknown never enters
	// here; a day takes the worst status among the hours that do have data.
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

// sla computes the uptime percentage over the retention window as the ratio of
// successful to total checks across all locations. Hours with no data simply
// contribute nothing. Returns an empty string when there is no data at all.
func (s *Server) sla(ctx context.Context, dash, svc string) string {
	now := time.Now().UTC()
	from := now.Add(-29 * 24 * time.Hour)
	rollups, _ := s.store.GetAggregateRollups(ctx, dash, svc, from, now)

	curHour := now.Truncate(time.Hour)
	if agg, ok := s.store.ComputeAggregate(ctx, dash, svc, curHour); ok {
		rollups[curHour.Unix()] = agg
	}

	var success, failure int
	for _, r := range rollups {
		success += r.Success
		failure += r.Failure
	}
	total := success + failure
	if total == 0 {
		return ""
	}
	pct := float64(success) / float64(total) * 100
	if pct == math.Trunc(pct) {
		return fmt.Sprintf("%.0f%%", pct)
	}
	return fmt.Sprintf("%.2f%%", pct)
}

type windowView struct {
	Locations string
	Message   string
	Start     string
	End       string
	Duration  string
	Count     int
	Ongoing   bool
}

type serviceData struct {
	Dashboard    string
	ServiceID    string
	Display      string
	Group        string
	Meta         store.Meta
	SLA          string
	SelectedDay  string
	WindowsTitle string
	Windows      []windowView
	Daily        []dayBar
}

func (s *Server) handleService(w http.ResponseWriter, r *http.Request) {
	dash := r.PathValue("dash")
	svc := r.PathValue("name")

	meta, _ := s.store.GetMeta(r.Context(), dash, svc)
	name := svc
	if meta.Name != "" {
		name = meta.Name
	}
	group, display := splitGroup(name)
	now := time.Now().UTC()

	// Default view: the rolling last 24 hours ending now. When a ?day=YYYY-MM-DD
	// is supplied, show that calendar day's 24 hours instead.
	from := now.Add(-24 * time.Hour).Truncate(time.Hour)
	to := now
	windowsTitle := "Issues (last 24 hours)"
	selectedDay := ""
	if d, err := time.Parse("2006-01-02", r.URL.Query().Get("day")); err == nil {
		from = d.UTC()
		to = from.Add(24*time.Hour - time.Second)
		windowsTitle = "Issues on " + from.Format("2006-01-02")
		selectedDay = from.Format("2006-01-02")
	}

	// A window left open at the end of the stream is only "ongoing" when the
	// view actually reaches the present.
	live := !to.Before(now.Truncate(time.Hour))
	windows := s.failureWindows(r.Context(), dash, svc, from, to, live)

	s.render(w, "service.html", serviceData{
		Dashboard:    dash,
		ServiceID:    svc,
		Display:      display,
		Group:        group,
		SLA:          s.sla(r.Context(), dash, svc),
		SelectedDay:  selectedDay,
		WindowsTitle: windowsTitle,
		Meta:         meta,
		Windows:      windows,
		Daily:        s.dailyBars(r.Context(), dash, svc),
	})
}

func (s *Server) failureWindows(ctx context.Context, dash, svc string, from, to time.Time, live bool) []windowView {
	raw := s.store.FailureWindows(ctx, dash, svc, from, to, live)
	out := make([]windowView, 0, len(raw))
	for _, w := range raw {
		msg := w.Message
		if msg == "" {
			msg = "(no message)"
		}
		start := time.Unix(w.Start, 0).UTC()
		end := time.Unix(w.End, 0).UTC()
		v := windowView{
			Locations: strings.Join(w.Locations, ", "),
			Message:   msg,
			Start:     start.Format("2006-01-02 15:04:05 UTC"),
			Count:     w.Count,
			Ongoing:   w.Ongoing,
		}
		if w.Ongoing {
			v.End = "ongoing"
			v.Duration = humanDuration(time.Now().UTC().Sub(start))
		} else {
			v.End = end.Format("2006-01-02 15:04:05 UTC")
			v.Duration = humanDuration(end.Sub(start))
		}
		out = append(out, v)
	}
	return out
}

func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	d = d.Round(time.Minute)
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	if h == 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh%dm", h, m)
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
		Service string     `json:"service"`
		Meta    store.Meta `json:"meta"`
		Daily   []dayBar   `json:"daily"`
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
