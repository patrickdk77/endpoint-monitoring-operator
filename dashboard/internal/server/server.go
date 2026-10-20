package server

import (
	"bytes"
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
	"log"
	"mime"
	"net/http"
	"path"
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

type staticAsset struct {
	content     []byte
	gzipped     []byte
	contentType string
}

type Server struct {
	store        *store.Store
	retention    int
	templates    *template.Template
	staticAssets map[string]staticAsset
}

func New(s *store.Store, retentionDays int) (*Server, error) {
	staticFSRoot, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}

	assets, err := loadStaticAssets(staticFSRoot)
	if err != nil {
		return nil, err
	}

	hashes := map[string]string{}
	for name, asset := range assets {
		hashes[name] = fmt.Sprintf("%08x", crc32.ChecksumIEEE(asset.content))
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
		"formatStatus": func(s string) string {
			return strings.ReplaceAll(s, "_", " ")
		},
		"assetURL": func(name string) string {
			return assetURL(name, hashes)
		},
		"barClass": func(status, date string) string {
			base := "bar-" + status
			if status == "" || status == "unknown" {
				return base
			}
			if parsed, err := time.Parse("2006-01-02", date); err == nil {
				cutoff := time.Now().UTC().AddDate(0, 0, -30)
				if parsed.UTC().Before(cutoff) {
					return base + " bar-old"
				}
			}
			return base
		},
	})
	var err2 error
	tmpl, err2 = tmpl.ParseFS(templateFS, "templates/*.html")
	if err2 != nil {
		return nil, err2
	}
	return &Server{store: s, retention: retentionDays, templates: tmpl, staticAssets: assets}, nil
}

func loadStaticAssets(fsys fs.FS) (map[string]staticAsset, error) {
	assets := map[string]staticAsset{}
	if err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		content, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		contentType := mime.TypeByExtension(path.Ext(name))
		if contentType == "" {
			contentType = http.DetectContentType(content)
		}
		var gzipBuf bytes.Buffer
		gz := gzip.NewWriter(&gzipBuf)
		if _, err := gz.Write(content); err != nil {
			gz.Close()
			return err
		}
		if err := gz.Close(); err != nil {
			return err
		}
		assets[name] = staticAsset{content: content, gzipped: gzipBuf.Bytes(), contentType: contentType}
		return nil
	}); err != nil {
		return nil, err
	}
	return assets, nil
}

func assetURL(name string, hashes map[string]string) string {
	if hash, ok := hashes[name]; ok {
		return "/static/" + name + "?v=" + hash
	}
	return "/static/" + name
}

func (s *Server) Handler() http.Handler {
	staticHandler := s.staticHandler()

	apiMux := http.NewServeMux()
	apiMux.HandleFunc("GET /dashboards", s.handleAPIDashboards)
	apiMux.HandleFunc("GET /dashboard/{dash}/overview", s.handleAPIOverview)
	apiMux.HandleFunc("GET /dashboard/{dash}/{name}", s.handleAPIService)
	apiMux.HandleFunc("GET /dashboard/{dash}/{name}/latency", s.handleAPILatency)

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

func (s *Server) staticHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := path.Clean(strings.TrimPrefix(r.URL.Path, "/static/"))
		clean = strings.TrimPrefix(clean, "/")
		if clean == "." || clean == "" || strings.HasPrefix(clean, "../") || strings.Contains(clean, "..") {
			http.NotFound(w, r)
			return
		}

		asset, ok := s.staticAssets[clean]
		if !ok {
			http.NotFound(w, r)
			return
		}

		selected := asset.content
		encoding := ""
		if isCompressible(asset.contentType) && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			selected = asset.gzipped
			encoding = "gzip"
		}

		w.Header().Set("Content-Type", asset.contentType)
		w.Header().Set("Content-Length", strconv.Itoa(len(selected)))
		if encoding != "" {
			w.Header().Set("Content-Encoding", encoding)
			w.Header().Add("Vary", "Accept-Encoding")
		}

		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write(selected)
	})
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
	Uptime string
	Failed int
	Total  int
	Color  template.CSS
}

// uptimeColor shades a day bar by its uptime on a scale spaced by nines: 100%
// is the operational color, 99.9% halfway to degraded, 99% degraded, 95%
// halfway to major_outage, and 90% or less major_outage.
func uptimeColor(success, total int) template.CSS {
	if success >= total {
		return "var(--operational)"
	}
	up := 100 * float64(success) / float64(total)
	anchors := []float64{100, 99.9, 99, 95, 90}
	step := float64(len(anchors) - 1)
	for i := 1; i < len(anchors); i++ {
		if up >= anchors[i] {
			step = float64(i-1) + (anchors[i-1]-up)/(anchors[i-1]-anchors[i])
			break
		}
	}
	from, to, mix := "operational", "degraded", step/2
	if step > 2 {
		from, to, mix = "degraded", "major_outage", (step-2)/2
	}
	return template.CSS(fmt.Sprintf("color-mix(in srgb, var(--%s) %.1f%%, var(--%s))", to, 100*mix, from))
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

func unavailable(w http.ResponseWriter, err error) {
	log.Printf("server: %v", err)
	http.Error(w, "status data is unavailable", http.StatusServiceUnavailable)
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	dash := r.PathValue("dash")
	services, err := s.store.ListServices(r.Context(), dash)
	if err != nil {
		unavailable(w, err)
		return
	}
	sort.Strings(services)
	now := time.Now().UTC()

	groupIndex := map[string]int{}
	var groups []serviceGroup
	for _, svc := range services {
		hist, err := s.store.History(r.Context(), dash, svc, now)
		if err != nil {
			unavailable(w, err)
			return
		}
		meta, _ := s.store.GetMeta(r.Context(), dash, svc)
		daily := s.dailyBars(hist, now)
		current := currentStatus(hist, now)
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
			SLA:     sla(hist, 30, now),
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

// currentStatus returns the most recent actual status for a service: the live
// current-hour aggregate if checks have run this hour, otherwise the most
// recently completed hour in the past 6 hours, otherwise unknown.
func currentStatus(hist store.History, now time.Time) status.AggregateStatus {
	since := now.Add(-store.StatusMaxAge).Unix()
	latestEpoch := int64(-1)
	for epoch := range hist.Hours {
		if epoch >= since && epoch > latestEpoch {
			latestEpoch = epoch
		}
	}
	if latestEpoch < 0 {
		return status.AggregateUnknown
	}
	return status.AggregateStatus(hist.Hours[latestEpoch].Status)
}

func (s *Server) dailyBars(hist store.History, now time.Time) []dayBar {
	// Render retention days ending with today (inclusive).
	from := now.AddDate(0, 0, -(s.retention - 1)).Truncate(24 * time.Hour)

	// Hours with no data are never stored or overlaid, so unknown never enters
	// here; a day takes the worst status among the hours that do have data.
	byDay := map[string]status.AggregateStatus{}
	for hourEpoch, rollup := range hist.Hours {
		if hourEpoch < from.Unix() {
			continue
		}
		day := time.Unix(hourEpoch, 0).UTC().Format("2006-01-02")
		st := status.AggregateStatus(rollup.Status)
		byDay[day] = status.WorstAggregate(byDay[day], st)
	}

	counts := hist.ByDay(now)
	var bars []dayBar
	for d := 0; d < s.retention; d++ {
		day := from.AddDate(0, 0, d)
		key := day.Format("2006-01-02")
		st := byDay[key]
		if st == "" {
			st = status.AggregateUnknown
		}
		bar := dayBar{
			Date:   key,
			Status: string(st),
			Label:  key,
		}
		if c := counts[key]; c.Total > 0 {
			bar.Uptime = store.FormatPercent(c.Success, c.Total)
			bar.Failed = c.Total - c.Success
			bar.Total = c.Total
			bar.Color = uptimeColor(c.Success, c.Total)
		}
		bars = append(bars, bar)
	}
	return bars
}

// sla is the uptime over the given number of days ending today, across all
// locations, or an empty string when there were no checks.
func sla(hist store.History, days int, now time.Time) string {
	return store.FormatPercent(hist.Uptime(days, now))
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
	SLA7         string
	SLA30        string
	SLA365       string
	SelectedDay  string
	Ranges       []rangeOption
	IssueRange   string
	WindowsTitle string
	Windows      []windowView
	Daily        []dayBar
}

// maxPlotPoints caps each location's series in the latency chart for ranges
// longer than a day; the one-day range plots every check.
const maxPlotPoints = 1440

type rangeOption struct {
	Key   string
	Label string
	Days  int
}

// rangeOptions lists the ranges the issues list and the chart offer: 24
// hours, 7 days and 30 days, then the whole retention, never past it.
func (s *Server) rangeOptions() []rangeOption {
	var opts []rangeOption
	for _, o := range []rangeOption{{"1d", "24 hours", 1}, {"7d", "7 days", 7}, {"30d", "30 days", 30}} {
		if o.Days <= s.retention {
			opts = append(opts, o)
		}
	}
	if last := opts[len(opts)-1]; s.retention > last.Days {
		opts = append(opts, rangeOption{strconv.Itoa(s.retention) + "d", strconv.Itoa(s.retention) + " days", s.retention})
	}
	return opts
}

func (s *Server) parseRange(key string) rangeOption {
	opts := s.rangeOptions()
	for _, o := range opts {
		if o.Key == key {
			return o
		}
	}
	return opts[0]
}

func (s *Server) handleAPILatency(w http.ResponseWriter, r *http.Request) {
	dash := r.PathValue("dash")
	svc := r.PathValue("name")
	rng := s.parseRange(r.URL.Query().Get("range"))
	maxPoints := maxPlotPoints
	if rng.Days <= 1 {
		maxPoints = 0
	}
	now := time.Now().UTC()
	lat, err := s.store.Latency(r.Context(), dash, svc, now.Add(-time.Duration(rng.Days)*24*time.Hour), now, maxPoints)
	if err != nil {
		unavailable(w, err)
		return
	}
	writeJSON(w, lat)
}

func (s *Server) handleService(w http.ResponseWriter, r *http.Request) {
	dash := r.PathValue("dash")
	svc := r.PathValue("name")

	now := time.Now().UTC()
	hist, err := s.store.History(r.Context(), dash, svc, now)
	if err != nil {
		unavailable(w, err)
		return
	}
	meta, _ := s.store.GetMeta(r.Context(), dash, svc)
	name := svc
	if meta.Name != "" {
		name = meta.Name
	}
	group, display := splitGroup(name)

	// Default view: the rolling range chosen with ?range= (24 hours unless
	// set) ending now. When a ?day=YYYY-MM-DD is supplied, show that calendar
	// day's 24 hours instead.
	issueRange := s.parseRange(r.URL.Query().Get("range"))
	from := now.Add(-time.Duration(issueRange.Days) * 24 * time.Hour).Truncate(time.Hour)
	to := now
	windowsTitle := "Issues (last " + issueRange.Label + ")"
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
	windows, err := s.failureWindows(r.Context(), dash, svc, from, to, now, live)
	if err != nil {
		unavailable(w, err)
		return
	}

	s.render(w, "service.html", serviceData{
		Dashboard:    dash,
		ServiceID:    svc,
		Display:      display,
		Group:        group,
		SLA7:         sla(hist, 7, now),
		SLA30:        sla(hist, 30, now),
		SLA365:       sla(hist, 365, now),
		SelectedDay:  selectedDay,
		Ranges:       s.rangeOptions(),
		IssueRange:   issueRange.Key,
		WindowsTitle: windowsTitle,
		Meta:         meta,
		Windows:      windows,
		Daily:        s.dailyBars(hist, now),
	})
}

func (s *Server) failureWindows(ctx context.Context, dash, svc string, from, to, now time.Time, live bool) ([]windowView, error) {
	raw, err := s.store.FailureWindows(ctx, dash, svc, from, to, now, live)
	if err != nil {
		return nil, err
	}
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
			v.Duration = humanDuration(now.Sub(start))
		} else {
			v.End = end.Format("2006-01-02 15:04:05 UTC")
			if d := end.Sub(start); d > 0 {
				v.Duration = humanDuration(d)
			}
		}
		out = append(out, v)
	}
	return out, nil
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
		unavailable(w, err)
		return
	}
	writeJSON(w, dashboards)
}

func (s *Server) handleAPIOverview(w http.ResponseWriter, r *http.Request) {
	dash := r.PathValue("dash")
	services, err := s.store.ListServices(r.Context(), dash)
	if err != nil {
		unavailable(w, err)
		return
	}
	type item struct {
		Service string     `json:"service"`
		Meta    store.Meta `json:"meta"`
		Daily   []dayBar   `json:"daily"`
	}
	now := time.Now().UTC()
	var out []item
	for _, svc := range services {
		hist, err := s.store.History(r.Context(), dash, svc, now)
		if err != nil {
			unavailable(w, err)
			return
		}
		meta, _ := s.store.GetMeta(r.Context(), dash, svc)
		out = append(out, item{Service: svc, Meta: meta, Daily: s.dailyBars(hist, now)})
	}
	writeJSON(w, out)
}

func (s *Server) handleAPIService(w http.ResponseWriter, r *http.Request) {
	dash := r.PathValue("dash")
	svc := r.PathValue("name")
	now := time.Now().UTC()
	hist, err := s.store.History(r.Context(), dash, svc, now)
	if err != nil {
		unavailable(w, err)
		return
	}
	meta, _ := s.store.GetMeta(r.Context(), dash, svc)
	writeJSON(w, map[string]any{
		"service": svc,
		"meta":    meta,
		"rollups": hist.Hours,
		"daily":   s.dailyBars(hist, now),
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
