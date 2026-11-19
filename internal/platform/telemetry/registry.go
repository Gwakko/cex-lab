// Package telemetry is a small metrics registry (counters, gauges, fixed-bucket
// histograms with labels) with a hand-written Prometheus text exposition, plus
// health and readiness handlers.
//
// Label values are passed on every call. Callers must keep label cardinality
// bounded: market, asset, command type, result - never user or order ids.
package telemetry

import (
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
)

var (
	metricNameRE = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelNameRE  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

// LatencyBuckets are upper bounds in seconds for in-process latencies.
var LatencyBuckets = []float64{0.00001, 0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1}

type kind string

const (
	counterKind   kind = "counter"
	gaugeKind     kind = "gauge"
	histogramKind kind = "histogram"
)

// Registry holds metric families. The zero value is not usable; use NewRegistry.
type Registry struct {
	mu       sync.Mutex
	families map[string]*family
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{families: map[string]*family{}}
}

type family struct {
	name    string
	help    string
	kind    kind
	labels  []string
	buckets []float64

	mu     sync.Mutex
	series map[string]*series
}

type series struct {
	labelValues []string
	value       float64  // counter, gauge
	buckets     []uint64 // histogram, non-cumulative per bucket
	sum         float64
	count       uint64
}

// register returns the existing family if the definition matches, so several
// components may share one metric. A conflicting definition is a programming
// error and panics.
func (r *Registry) register(name, help string, k kind, buckets []float64, labels []string) *family {
	if !metricNameRE.MatchString(name) {
		panic(fmt.Sprintf("telemetry: invalid metric name %q", name))
	}
	for _, l := range labels {
		if !labelNameRE.MatchString(l) || strings.HasPrefix(l, "__") || (k == histogramKind && l == "le") {
			panic(fmt.Sprintf("telemetry: invalid label name %q on %s", l, name))
		}
	}
	if k == histogramKind && (len(buckets) == 0 || !slices.IsSorted(buckets)) {
		panic(fmt.Sprintf("telemetry: %s needs sorted buckets", name))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if f, ok := r.families[name]; ok {
		if f.kind != k || !slices.Equal(f.labels, labels) || !slices.Equal(f.buckets, buckets) {
			panic(fmt.Sprintf("telemetry: %s registered twice with different definitions", name))
		}
		return f
	}
	f := &family{name: name, help: help, kind: k, labels: labels, buckets: buckets, series: map[string]*series{}}
	r.families[name] = f
	return f
}

// update finds or creates the series for labelValues and runs fn under the lock.
func (f *family) update(labelValues []string, fn func(*series)) {
	if len(labelValues) != len(f.labels) {
		panic(fmt.Sprintf("telemetry: %s wants %d label values, got %d", f.name, len(f.labels), len(labelValues)))
	}
	key := strings.Join(labelValues, "\xff")
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.series[key]
	if !ok {
		s = &series{labelValues: slices.Clone(labelValues)}
		if f.kind == histogramKind {
			s.buckets = make([]uint64, len(f.buckets))
		}
		f.series[key] = s
	}
	fn(s)
}

// Counter only goes up.
type Counter struct{ f *family }

// Counter registers (or returns) a counter family.
func (r *Registry) Counter(name, help string, labels ...string) *Counter {
	return &Counter{r.register(name, help, counterKind, nil, labels)}
}

// Add increases the counter. Negative values panic.
func (c *Counter) Add(v float64, labelValues ...string) {
	if v < 0 {
		panic("telemetry: counter decreased: " + c.f.name)
	}
	c.f.update(labelValues, func(s *series) { s.value += v })
}

// Inc adds 1.
func (c *Counter) Inc(labelValues ...string) { c.Add(1, labelValues...) }

// Gauge can go up and down.
type Gauge struct{ f *family }

// Gauge registers (or returns) a gauge family.
func (r *Registry) Gauge(name, help string, labels ...string) *Gauge {
	return &Gauge{r.register(name, help, gaugeKind, nil, labels)}
}

// Set sets the gauge.
func (g *Gauge) Set(v float64, labelValues ...string) {
	g.f.update(labelValues, func(s *series) { s.value = v })
}

// Histogram counts observations into fixed buckets.
type Histogram struct{ f *family }

// Histogram registers (or returns) a histogram family with the given upper bounds.
func (r *Registry) Histogram(name, help string, buckets []float64, labels ...string) *Histogram {
	return &Histogram{r.register(name, help, histogramKind, buckets, labels)}
}

// Observe records one value.
func (h *Histogram) Observe(v float64, labelValues ...string) {
	h.f.update(labelValues, func(s *series) {
		if i, _ := slices.BinarySearch(h.f.buckets, v); i < len(s.buckets) {
			s.buckets[i]++
		}
		s.sum += v
		s.count++
	})
}

// Get returns a counter or gauge value, or a histogram's observation count.
// It is meant for tests and health checks.
func (r *Registry) Get(name string, labelValues ...string) (float64, bool) {
	r.mu.Lock()
	f, ok := r.families[name]
	r.mu.Unlock()
	if !ok {
		return 0, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.series[strings.Join(labelValues, "\xff")]
	if !ok {
		return 0, false
	}
	if f.kind == histogramKind {
		return float64(s.count), true
	}
	return s.value, true
}

// WriteText writes all metrics in the Prometheus text exposition format
// (version 0.0.4), families and series sorted for stable output.
func (r *Registry) WriteText(w io.Writer) error {
	r.mu.Lock()
	fams := make([]*family, 0, len(r.families))
	for _, f := range r.families {
		fams = append(fams, f)
	}
	r.mu.Unlock()
	slices.SortFunc(fams, func(a, b *family) int { return strings.Compare(a.name, b.name) })

	var b strings.Builder
	for _, f := range fams {
		f.writeText(&b)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func (f *family) writeText(b *strings.Builder) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", f.name, escapeHelp(f.help), f.name, f.kind)
	keys := make([]string, 0, len(f.series))
	for k := range f.series {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		s := f.series[k]
		if f.kind != histogramKind {
			fmt.Fprintf(b, "%s%s %s\n", f.name, f.labelPairs(s.labelValues, ""), formatFloat(s.value))
			continue
		}
		var cum uint64
		for i, ub := range f.buckets {
			cum += s.buckets[i]
			fmt.Fprintf(b, "%s_bucket%s %d\n", f.name, f.labelPairs(s.labelValues, formatFloat(ub)), cum)
		}
		fmt.Fprintf(b, "%s_bucket%s %d\n", f.name, f.labelPairs(s.labelValues, "+Inf"), s.count)
		fmt.Fprintf(b, "%s_sum%s %s\n", f.name, f.labelPairs(s.labelValues, ""), formatFloat(s.sum))
		fmt.Fprintf(b, "%s_count%s %d\n", f.name, f.labelPairs(s.labelValues, ""), s.count)
	}
}

func (f *family) labelPairs(values []string, le string) string {
	if len(values) == 0 && le == "" {
		return ""
	}
	pairs := make([]string, 0, len(values)+1)
	for i, v := range values {
		pairs = append(pairs, f.labels[i]+`="`+escapeLabel(v)+`"`)
	}
	if le != "" {
		pairs = append(pairs, `le="`+le+`"`)
	}
	return "{" + strings.Join(pairs, ",") + "}"
}

var (
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
	labelEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`)
)

func escapeHelp(s string) string  { return helpEscaper.Replace(s) }
func escapeLabel(s string) string { return labelEscaper.Replace(s) }

func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	case v == math.Trunc(v) && math.Abs(v) < 1<<53:
		return strconv.FormatInt(int64(v), 10) // exact integers read better than 3.5e+06
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
