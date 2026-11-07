// Package metrics is a tiny Prometheus text-format registry (counters + gauge
// callbacks); it avoids pulling a client library into the build.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"sync"
)

type Sample struct {
	Labels string // e.g. `channel="sms",status="pending"`
	Value  float64
}

type Registry struct {
	mu       sync.Mutex
	counters map[string]map[string]float64 // name -> labels -> value
	help     map[string]string
	gauges   map[string]func() []Sample
}

func New() *Registry {
	return &Registry{counters: map[string]map[string]float64{}, help: map[string]string{}, gauges: map[string]func() []Sample{}}
}

func (r *Registry) Describe(name, help string) {
	r.mu.Lock()
	r.help[name] = help
	r.mu.Unlock()
}

// Inc adds 1 to the counter series name{labels}.
func (r *Registry) Inc(name, labels string) { r.Add(name, labels, 1) }

func (r *Registry) Add(name, labels string, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.counters[name] == nil {
		r.counters[name] = map[string]float64{}
	}
	r.counters[name][labels] += v
}

// Value returns the current counter value (used by tests).
func (r *Registry) Value(name, labels string) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counters[name][labels]
}

func (r *Registry) Gauge(name string, fn func() []Sample) {
	r.mu.Lock()
	r.gauges[name] = fn
	r.mu.Unlock()
}

func (r *Registry) WritePrometheus(w io.Writer) {
	r.mu.Lock()
	names := make([]string, 0, len(r.counters))
	for n := range r.counters {
		names = append(names, n)
	}
	sort.Strings(names)
	type series struct {
		labels string
		v      float64
	}
	snap := map[string][]series{}
	for _, n := range names {
		for l, v := range r.counters[n] {
			snap[n] = append(snap[n], series{l, v})
		}
		sort.Slice(snap[n], func(i, j int) bool { return snap[n][i].labels < snap[n][j].labels })
	}
	help := map[string]string{}
	for k, v := range r.help {
		help[k] = v
	}
	gnames := make([]string, 0, len(r.gauges))
	gfns := map[string]func() []Sample{}
	for n, fn := range r.gauges {
		gnames = append(gnames, n)
		gfns[n] = fn
	}
	r.mu.Unlock()
	sort.Strings(gnames)

	for _, n := range names {
		if h := help[n]; h != "" {
			fmt.Fprintf(w, "# HELP %s %s\n", n, h)
		}
		fmt.Fprintf(w, "# TYPE %s counter\n", n)
		for _, s := range snap[n] {
			writeSeries(w, n, s.labels, s.v)
		}
	}
	for _, n := range gnames {
		if h := help[n]; h != "" {
			fmt.Fprintf(w, "# HELP %s %s\n", n, h)
		}
		fmt.Fprintf(w, "# TYPE %s gauge\n", n)
		for _, s := range gfns[n]() {
			writeSeries(w, n, s.Labels, s.Value)
		}
	}
}

func writeSeries(w io.Writer, name, labels string, v float64) {
	if labels == "" {
		fmt.Fprintf(w, "%s %g\n", name, v)
		return
	}
	fmt.Fprintf(w, "%s{%s} %g\n", name, labels, v)
}
