package telemetry

import (
	"bufio"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// exposition is a parsed Prometheus text page.
type exposition struct {
	help    map[string]string
	types   map[string]string
	samples map[string]float64 // key: name{labels} exactly as written
	order   []string           // metric names in TYPE order
}

// parse is a strict parser for the subset of the format we emit: every sample
// must belong to a family announced by HELP and TYPE lines just before it.
func parse(t *testing.T, text string) exposition {
	t.Helper()
	e := exposition{help: map[string]string{}, types: map[string]string{}, samples: map[string]float64{}}
	current := ""
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "# HELP "):
			name, help, _ := strings.Cut(strings.TrimPrefix(line, "# HELP "), " ")
			e.help[name] = help
		case strings.HasPrefix(line, "# TYPE "):
			name, typ, _ := strings.Cut(strings.TrimPrefix(line, "# TYPE "), " ")
			if _, ok := e.help[name]; !ok {
				t.Fatalf("TYPE before HELP for %s", name)
			}
			e.types[name] = typ
			e.order = append(e.order, name)
			current = name
		default:
			i := strings.LastIndexByte(line, ' ')
			if i < 0 {
				t.Fatalf("malformed sample %q", line)
			}
			key, raw := line[:i], line[i+1:]
			name := key
			if j := strings.IndexByte(key, '{'); j >= 0 {
				name = key[:j]
				if !strings.HasSuffix(key, "}") {
					t.Fatalf("unterminated labels in %q", line)
				}
			}
			base := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(name, "_bucket"), "_sum"), "_count")
			if name != current && base != current {
				t.Fatalf("sample %s outside its family %s", name, current)
			}
			v, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				t.Fatalf("bad value in %q: %v", line, err)
			}
			e.samples[key] = v
		}
	}
	return e
}

func render(t *testing.T, r *Registry) exposition {
	t.Helper()
	var b strings.Builder
	if err := r.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	return parse(t, b.String())
}

func TestCounterGaugeExposition(t *testing.T) {
	r := NewRegistry()
	orders := r.Counter("orders_total", "Orders placed.", "market", "side")
	orders.Inc("BTC-USDT", "buy")
	orders.Inc("BTC-USDT", "buy")
	orders.Add(3, "ETH-USDT", "sell")
	r.Gauge("queue_depth", "Commands waiting.").Set(7)

	e := render(t, r)
	if e.types["orders_total"] != "counter" || e.types["queue_depth"] != "gauge" {
		t.Fatalf("types %v", e.types)
	}
	if e.help["orders_total"] != "Orders placed." {
		t.Fatalf("help %q", e.help["orders_total"])
	}
	want := map[string]float64{
		`orders_total{market="BTC-USDT",side="buy"}`:  2,
		`orders_total{market="ETH-USDT",side="sell"}`: 3,
		`queue_depth`: 7,
	}
	for k, v := range want {
		if e.samples[k] != v {
			t.Errorf("%s = %v, want %v", k, e.samples[k], v)
		}
	}
	if strings.Join(e.order, ",") != "orders_total,queue_depth" {
		t.Fatalf("families not sorted: %v", e.order)
	}
}

func TestHistogramBucketsAreCumulative(t *testing.T) {
	r := NewRegistry()
	h := r.Histogram("latency_seconds", "Latency.", []float64{0.1, 0.5, 1}, "op")
	for _, v := range []float64{0.05, 0.1, 0.3, 0.7, 2, 3} {
		h.Observe(v, "place")
	}
	e := render(t, r)
	want := map[string]float64{
		`latency_seconds_bucket{op="place",le="0.1"}`:  2, // 0.05 and the inclusive 0.1
		`latency_seconds_bucket{op="place",le="0.5"}`:  3,
		`latency_seconds_bucket{op="place",le="1"}`:    4,
		`latency_seconds_bucket{op="place",le="+Inf"}`: 6,
		`latency_seconds_count{op="place"}`:            6,
		`latency_seconds_sum{op="place"}`:              6.15,
	}
	for k, v := range want {
		got, ok := e.samples[k]
		if !ok || fmt.Sprintf("%.6f", got) != fmt.Sprintf("%.6f", v) {
			t.Errorf("%s = %v (present %v), want %v", k, got, ok, v)
		}
	}
	if e.types["latency_seconds"] != "histogram" {
		t.Fatal("histogram TYPE missing")
	}
	if n, _ := r.Get("latency_seconds", "place"); n != 6 {
		t.Fatalf("Get on histogram = %v", n)
	}
}

func TestEscaping(t *testing.T) {
	r := NewRegistry()
	r.Counter("weird_total", "line one\nback\\slash", "reason").Inc("say \"no\"\n\\")
	var b strings.Builder
	r.WriteText(&b)
	out := b.String()
	if !strings.Contains(out, `# HELP weird_total line one\nback\\slash`+"\n") {
		t.Fatalf("help not escaped:\n%s", out)
	}
	if !strings.Contains(out, `weird_total{reason="say \"no\"\n\\"} 1`+"\n") {
		t.Fatalf("label not escaped:\n%s", out)
	}
	parse(t, out)
}

func TestRegistrationRules(t *testing.T) {
	r := NewRegistry()
	a := r.Counter("shared_total", "Shared.", "market")
	b := r.Counter("shared_total", "Shared.", "market")
	a.Inc("x")
	b.Inc("x")
	if v, _ := r.Get("shared_total", "x"); v != 2 {
		t.Fatalf("same definition must share series, got %v", v)
	}
	mustPanic(t, "type conflict", func() { r.Gauge("shared_total", "Shared.", "market") })
	mustPanic(t, "label conflict", func() { r.Counter("shared_total", "Shared.", "asset") })
	mustPanic(t, "bad name", func() { r.Counter("bad-name", "") })
	mustPanic(t, "le label", func() { r.Histogram("h", "", []float64{1}, "le") })
	mustPanic(t, "unsorted buckets", func() { r.Histogram("h2", "", []float64{2, 1}) })
	mustPanic(t, "label arity", func() { a.Inc() })
	mustPanic(t, "negative counter", func() { a.Add(-1, "x") })
	if _, ok := r.Get("missing"); ok {
		t.Fatal("missing metric reported present")
	}
}

func mustPanic(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s: expected panic", name)
		}
	}()
	fn()
}

func TestConcurrentUpdatesAndScrapes(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("c_total", "", "market")
	g := r.Gauge("g", "", "market")
	h := r.Histogram("h_seconds", "", LatencyBuckets, "market")
	markets := []string{"a", "b", "c"}
	const workers, perWorker = 8, 1000
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := markets[w%len(markets)]
			for i := range perWorker {
				c.Inc(m)
				g.Set(float64(i), m)
				h.Observe(0.001, m)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 50 {
			var b strings.Builder
			r.WriteText(&b)
		}
	}()
	wg.Wait()
	var total, obs float64
	for _, m := range markets {
		v, _ := r.Get("c_total", m)
		n, _ := r.Get("h_seconds", m)
		total += v
		obs += n
	}
	if total != workers*perWorker || obs != workers*perWorker {
		t.Fatalf("lost updates: counter %v, histogram %v", total, obs)
	}
}

func BenchmarkCounterInc(b *testing.B) {
	c := NewRegistry().Counter("bench_total", "", "market", "result")
	for b.Loop() {
		c.Inc("BTC-USDT", "applied")
	}
}

func BenchmarkHistogramObserve(b *testing.B) {
	h := NewRegistry().Histogram("bench_seconds", "", LatencyBuckets, "market")
	for b.Loop() {
		h.Observe(0.0003, "BTC-USDT")
	}
}

func TestFormatFloat(t *testing.T) {
	for v, want := range map[float64]string{
		3_500_000: "3500000", 0: "0", -2: "-2", 0.25: "0.25", 1e300: "1e+300",
		math.Inf(1): "+Inf", math.Inf(-1): "-Inf", math.NaN(): "NaN",
	} {
		if got := formatFloat(v); got != want {
			t.Errorf("formatFloat(%v) = %q, want %q", v, got, want)
		}
	}
}
