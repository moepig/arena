package telemetry

// OpenMetrics /metrics endpoint, emitted in parallel with EMF.
// Dependency-free by design: the arena metric set is small and fully
// known, so a last-value gauge registry plus running totals for count-like
// events covers the Prometheus/Grafana use case without client_golang.

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"unicode"
)

// PromExporter accumulates metric observations and serves them in the
// Prometheus text exposition format.
type PromExporter struct {
	mu     sync.Mutex
	gauges map[string]promSample // key = name + label signature
}

type promSample struct {
	name   string
	labels string // rendered {k="v",...} or ""
	value  float64
}

// NewPromExporter returns an empty exporter; attach it to an Emitter.
func NewPromExporter() *PromExporter {
	return &PromExporter{gauges: map[string]promSample{}}
}

// observe records the latest value per (metric, dimension) pair.
func (p *PromExporter) observe(namespace string, dims map[string]string, data ...Datum) {
	if p == nil {
		return
	}
	labels := renderLabels(dims)
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, d := range data {
		name := promName(namespace, d.Name, d.Unit)
		key := name + labels
		p.gauges[key] = promSample{name: name, labels: labels, value: d.Value}
	}
}

// ServeHTTP renders the current samples (text/plain exposition format).
func (p *PromExporter) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	p.mu.Lock()
	samples := make([]promSample, 0, len(p.gauges))
	for _, s := range p.gauges {
		samples = append(samples, s)
	}
	p.mu.Unlock()

	sort.Slice(samples, func(i, j int) bool {
		if samples[i].name != samples[j].name {
			return samples[i].name < samples[j].name
		}
		return samples[i].labels < samples[j].labels
	})
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	seen := map[string]bool{}
	for _, s := range samples {
		if !seen[s.name] {
			fmt.Fprintf(w, "# TYPE %s gauge\n", s.name)
			seen[s.name] = true
		}
		fmt.Fprintf(w, "%s%s %g\n", s.name, s.labels, s.value)
	}
}

// promName builds "arena_fleet_ready_game_servers"-style names from the EMF
// namespace + CamelCase metric name; duration metrics keep their unit
// visible ("..._milliseconds").
func promName(namespace, metric string, unit Unit) string {
	ns := strings.ToLower(strings.ReplaceAll(namespace, "/", "_"))
	name := ns + "_" + snake(metric)
	if unit == UnitMilliseconds && !strings.HasSuffix(name, "_milliseconds") {
		name += "_milliseconds"
	}
	return name
}

func snake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func renderLabels(dims map[string]string) string {
	if len(dims) == 0 {
		return ""
	}
	keys := make([]string, 0, len(dims))
	for k := range dims {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%q", snake(k), dims[k]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}
