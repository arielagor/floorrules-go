// Package metrics is a deliberately small Prometheus text-format counter
// registry. A production service would use prometheus/client_golang (or
// OpenTelemetry); this keeps the sample's dependency surface to pgx alone.
package metrics

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// Registry holds counters and gauges keyed by name and label set.
type Registry struct {
	mu       sync.Mutex
	help     map[string]string
	counters map[string]map[string]float64 // name -> rendered labels -> value
	gauges   map[string]bool               // names written with Set
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{help: map[string]string{}, counters: map[string]map[string]float64{}, gauges: map[string]bool{}}
}

// Set sets a gauge to v. A name is a gauge or a counter, never both. A nil
// registry is a no-op.
func (r *Registry) Set(name string, v float64, labels ...string) {
	if r == nil {
		return
	}
	key := renderLabels(labels)
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.counters[name]
	if m == nil {
		m = map[string]float64{}
		r.counters[name] = m
	}
	r.gauges[name] = true
	m[key] = v
}

// Describe sets the HELP text for a counter.
func (r *Registry) Describe(name, help string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.help[name] = help
	if r.counters[name] == nil {
		r.counters[name] = map[string]float64{}
	}
}

// Inc adds one to a counter. labels are key, value pairs.
func (r *Registry) Inc(name string, labels ...string) { r.Add(name, 1, labels...) }

// Add adds v to a counter. A nil registry is a no-op, so callers that don't
// care about metrics can pass nil.
func (r *Registry) Add(name string, v float64, labels ...string) {
	if r == nil {
		return
	}
	key := renderLabels(labels)
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.counters[name]
	if m == nil {
		m = map[string]float64{}
		r.counters[name] = m
	}
	m[key] += v
}

// Get returns a counter's current value (for tests).
func (r *Registry) Get(name string, labels ...string) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counters[name][renderLabels(labels)]
}

func renderLabels(labels []string) string {
	if len(labels) == 0 {
		return ""
	}
	parts := make([]string, 0, len(labels)/2)
	for i := 0; i+1 < len(labels); i += 2 {
		parts = append(parts, fmt.Sprintf(`%s="%s"`, labels[i], escape(labels[i+1])))
	}
	sort.Strings(parts)
	return "{" + strings.Join(parts, ",") + "}"
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func escape(v string) string { return labelEscaper.Replace(v) }

// WriteText renders every counter and gauge in the Prometheus text exposition format,
// sorted for stable output.
func (r *Registry) WriteText(w io.Writer) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.counters))
	for n := range r.counters {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if h := r.help[n]; h != "" {
			if _, err := fmt.Fprintf(w, "# HELP %s %s\n", n, h); err != nil {
				return err
			}
		}
		typ := "counter"
		if r.gauges[n] {
			typ = "gauge"
		}
		if _, err := fmt.Fprintf(w, "# TYPE %s %s\n", n, typ); err != nil {
			return err
		}
		keys := make([]string, 0, len(r.counters[n]))
		for k := range r.counters[n] {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if _, err := fmt.Fprintf(w, "%s%s %g\n", n, k, r.counters[n][k]); err != nil {
				return err
			}
		}
	}
	return nil
}

// ServeHTTP exposes the registry at /metrics.
func (r *Registry) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_ = r.WriteText(w)
}
