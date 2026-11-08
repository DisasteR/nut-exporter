// Copyright 2026 Benjamin Saiz
// SPDX-License-Identifier: Apache-2.0

package export

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/DisasteR/nut-exporter/internal/classify"
	"github.com/DisasteR/nut-exporter/internal/collect"
	"github.com/DisasteR/nut-exporter/internal/config"
)

// Collector is what the exporter needs from a server collector.
type Collector interface {
	Collect(ctx context.Context) collect.Server
}

// Result is one shared collection and its projection.
type Result struct {
	At      time.Time
	Servers []collect.Server
	samples []sample
	varSup  map[string]string // server\x00ups\x00variable -> suppression reason
}

type call struct {
	done chan struct{}
	res  *Result
}

// Exporter coordinates collections and serves /metrics and /inventory.
type Exporter struct {
	cfg        config.Config
	collectors []Collector
	proj       projector
	version    string
	log        *slog.Logger

	mu       sync.Mutex
	inflight *call
	now      func() time.Time
}

// New returns an exporter over the given collectors, one per configured server, in order.
func New(cfg config.Config, collectors []Collector, version string, log *slog.Logger) (*Exporter, error) {
	cls, err := classify.New(cfg.Classify.Numeric, cfg.Classify.Text)
	if err != nil {
		return nil, err
	}
	return &Exporter{
		cfg: cfg, collectors: collectors, version: version, log: log, now: time.Now,
		proj: projector{cfg: cfg, cls: cls, st: newState()},
	}, nil
}

// Collect returns the result of the in-flight collection, or starts one. The collection owns its
// deadline (timeouts.scrape); ctx only bounds how long this caller waits (design §3).
func (e *Exporter) Collect(ctx context.Context) (*Result, error) {
	e.mu.Lock()
	c := e.inflight
	if c == nil {
		c = &call{done: make(chan struct{})}
		e.inflight = c
		go e.run(c)
	}
	e.mu.Unlock()
	select {
	case <-c.done:
		return c.res, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (e *Exporter) run(c *call) {
	ctx, cancel := context.WithTimeout(context.Background(), e.cfg.Timeouts.Scrape)
	defer cancel()
	servers := make([]collect.Server, len(e.collectors))
	var wg sync.WaitGroup
	for i, col := range e.collectors {
		wg.Add(1)
		go func() {
			defer wg.Done()
			servers[i] = col.Collect(ctx)
		}()
	}
	wg.Wait()
	p := e.proj
	p.now = e.now()
	p.varSup = map[string]string{}
	res := &Result{At: p.now, Servers: servers, samples: p.project(servers), varSup: p.varSup}
	e.mu.Lock()
	c.res = res
	e.inflight = nil
	e.mu.Unlock()
	close(c.done)
}

// metricsCollector emits the samples of one result.
type metricsCollector struct {
	res     *Result
	version string
	log     *slog.Logger
}

func (m metricsCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, f := range families {
		ch <- f.desc()
	}
}

func (m metricsCollector) Collect(ch chan<- prometheus.Metric) {
	descs := map[*family]*prometheus.Desc{}
	for _, s := range m.res.samples {
		d := descs[s.f]
		if d == nil {
			d = s.f.desc()
			descs[s.f] = d
		}
		// The projection only emits valid labels; an error here is a bug, logged, never a panic
		// (Collect runs in registry goroutines, a panic would stop the process).
		if mt, err := prometheus.NewConstMetric(d, s.f.valueType(), s.value, s.labels...); err == nil {
			ch <- mt
		} else {
			m.log.Error("dropped series", "family", s.f.name, "err", err)
		}
	}
	if mt, err := prometheus.NewConstMetric(fBuildInfo.desc(), prometheus.GaugeValue, 1, m.version, runtime.Version()); err == nil {
		ch <- mt
	}
}

// ServeMetrics serves /metrics.
func (e *Exporter) ServeMetrics(w http.ResponseWriter, r *http.Request) {
	res, err := e.Collect(r.Context())
	if err != nil {
		http.Error(w, "collection not finished before the request ended", http.StatusServiceUnavailable)
		return
	}
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(metricsCollector{res: res, version: e.version, log: e.log}); err != nil {
		e.log.Error("register", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	promhttp.HandlerFor(reg, promhttp.HandlerOpts{ErrorLog: slogAdapter{e.log}, ErrorHandling: promhttp.ContinueOnError}).ServeHTTP(w, r)
}

// ServeInventory serves /inventory (design §6b).
func (e *Exporter) ServeInventory(w http.ResponseWriter, r *http.Request) {
	res, err := e.Collect(r.Context())
	if err != nil {
		http.Error(w, "collection not finished before the request ended", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(e.inventory(res)); err != nil {
		e.log.Error("inventory", "err", err)
	}
}

type slogAdapter struct{ l *slog.Logger }

func (a slogAdapter) Println(v ...any) { a.l.Error("metrics", "err", v) }
