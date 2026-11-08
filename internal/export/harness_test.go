package export_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/DisasteR/nut-exporter/internal/collect"
	"github.com/DisasteR/nut-exporter/internal/config"
	"github.com/DisasteR/nut-exporter/internal/export"
	"github.com/DisasteR/nut-exporter/internal/fakeupsd"
	"github.com/DisasteR/nut-exporter/internal/nut"
)

// upsSpec scripts one UPS of a fake upsd (answers follow NUT 2.7.4 net-protocol).
type upsSpec struct {
	name, desc string
	rawVars    []string // full LIST VAR answer incl. BEGIN/END, sent verbatim when non-nil
	vars       []string // VAR lines (without BEGIN/END)
	logins     int
	clients    []string
	cmds       []string
	rw         []string // RW lines
	typ        map[string]string
	descs      map[string]string
	cmdDescs   map[string]string
	enum       map[string][]string
	rng        map[string][][2]string
	override   map[string]fakeupsd.Response // by exact request line
	delay      map[string]time.Duration     // by exact request line
	hangAll    bool                         // every per-UPS command hangs
	hangMeta   bool                         // GET TYPE/DESC/CMDDESC, LIST ENUM/RANGE hang
}

func q(s string) string { return strconv.Quote(s) }

// nutq quotes a value as upsd does: only \" and \\ are escaped, other bytes verbatim.
func nutq(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

func varLine(ups, name, value string) string { return "VAR " + ups + " " + name + " " + nutq(value) }

// newUPS builds a spec from name/value pairs.
func newUPS(name string, kv ...string) *upsSpec {
	u := &upsSpec{name: name, desc: "desc " + name}
	for i := 0; i+1 < len(kv); i += 2 {
		u.vars = append(u.vars, varLine(name, kv[i], kv[i+1]))
	}
	return u
}

type fake struct {
	mu       sync.Mutex
	ups      []*upsSpec
	ver      string
	netver   string
	override map[string]fakeupsd.Response
	srv      *fakeupsd.Server
}

var allowedPrefixes = []string{"VER", "NETVER", "LIST UPS", "LIST VAR ", "LIST CMD ", "LIST RW ", "LIST CLIENT ",
	"LIST ENUM ", "LIST RANGE ", "GET NUMLOGINS ", "GET TYPE ", "GET DESC ", "GET CMDDESC ", "LOGOUT"}

// Catches an implementation that sends a command outside design section 3's list, or any mutating one.
func checkRequests(t *testing.T, reqs []string) {
	t.Helper()
	for _, r := range reqs {
		if fakeupsd.Mutating(r) {
			t.Errorf("mutating request sent: %q", r)
		}
		ok := false
		for _, p := range allowedPrefixes {
			if r == strings.TrimSpace(p) || strings.HasPrefix(r, p) {
				ok = true
			}
		}
		if !ok {
			t.Errorf("request outside design section 3 list: %q", r)
		}
	}
}

func newFake(t *testing.T, ups ...*upsSpec) *fake {
	t.Helper()
	f := &fake{ups: ups, ver: "Network UPS Tools upsd 2.7.4 - http://www.networkupstools.org/", netver: "1.2",
		override: map[string]fakeupsd.Response{}}
	f.srv = fakeupsd.New(t, f.handle)
	t.Cleanup(func() { checkRequests(t, f.srv.Requests()) })
	return f
}

func (f *fake) update(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func (f *fake) requests() []string { return f.srv.Requests() }

func (f *fake) count(prefix string) int {
	n := 0
	for _, r := range f.requests() {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

func (f *fake) handle(line string) fakeupsd.Response {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.override[line]; ok {
		return r
	}
	w := strings.Fields(line)
	switch {
	case line == "VER":
		return fakeupsd.Response{Lines: []string{f.ver}}
	case line == "NETVER":
		return fakeupsd.Response{Lines: []string{f.netver}}
	case line == "LOGOUT":
		return fakeupsd.Response{Lines: []string{"OK Goodbye"}}
	case line == "LIST UPS":
		l := []string{"BEGIN LIST UPS"}
		for _, u := range f.ups {
			l = append(l, "UPS "+u.name+" "+nutq(u.desc))
		}
		return fakeupsd.Response{Lines: append(l, "END LIST UPS")}
	}
	idx := 2
	if len(w) < 3 {
		return fakeupsd.Response{Lines: []string{"ERR UNKNOWN-COMMAND"}}
	}
	var u *upsSpec
	for _, x := range f.ups {
		if x.name == w[idx] {
			u = x
		}
	}
	if u == nil {
		return fakeupsd.Response{Lines: []string{"ERR UNKNOWN-UPS"}}
	}
	if r, ok := u.override[line]; ok {
		return r
	}
	meta := w[1] == "TYPE" || w[1] == "DESC" || w[1] == "CMDDESC" || w[1] == "ENUM" || w[1] == "RANGE"
	if u.hangAll || (u.hangMeta && meta) {
		return fakeupsd.Response{Hang: true}
	}
	r := fakeupsd.Response{Delay: u.delay[line]}
	list := func(kind string, items []string) []string {
		l := []string{"BEGIN LIST " + strings.Join(w[1:], " ")}
		_ = kind
		l = append(l, items...)
		return append(l, "END LIST "+strings.Join(w[1:], " "))
	}
	cmd := w[0] + " " + w[1]
	switch cmd {
	case "LIST VAR":
		if u.rawVars != nil {
			r.Lines = u.rawVars
		} else {
			r.Lines = list("VAR", u.vars)
		}
	case "GET NUMLOGINS":
		r.Lines = []string{fmt.Sprintf("NUMLOGINS %s %d", u.name, u.logins)}
	case "LIST CLIENT":
		var l []string
		for _, c := range u.clients {
			l = append(l, "CLIENT "+u.name+" "+c)
		}
		r.Lines = list("CLIENT", l)
	case "LIST CMD":
		var l []string
		for _, c := range u.cmds {
			l = append(l, "CMD "+u.name+" "+c)
		}
		r.Lines = list("CMD", l)
	case "LIST RW":
		r.Lines = list("RW", u.rw)
	case "GET TYPE":
		t := u.typ[w[3]]
		if t == "" {
			t = "NUMBER"
		}
		r.Lines = []string{"TYPE " + u.name + " " + w[3] + " " + t}
	case "GET DESC":
		d, ok := u.descs[w[3]]
		if !ok {
			d = "Description of " + w[3]
		}
		r.Lines = []string{"DESC " + u.name + " " + w[3] + " " + nutq(d)}
	case "GET CMDDESC":
		d, ok := u.cmdDescs[w[3]]
		if !ok {
			d = "Description of " + w[3]
		}
		r.Lines = []string{"CMDDESC " + u.name + " " + w[3] + " " + nutq(d)}
	case "LIST ENUM":
		var l []string
		for _, e := range u.enum[w[3]] {
			l = append(l, "ENUM "+u.name+" "+w[3]+" "+nutq(e))
		}
		r.Lines = list("ENUM", l)
	case "LIST RANGE":
		var l []string
		for _, e := range u.rng[w[3]] {
			l = append(l, "RANGE "+u.name+" "+w[3]+" "+nutq(e[0])+" "+nutq(e[1]))
		}
		r.Lines = list("RANGE", l)
	default:
		r.Lines = []string{"ERR UNKNOWN-COMMAND"}
	}
	return r
}

type named struct {
	name string
	f    *fake
	addr string // used when f is nil
}

type env struct {
	t    *testing.T
	exp  *export.Exporter
	cols []*collect.ServerCollector
	cfg  config.Config
}

func baseCfg() config.Config {
	c := config.Default()
	c.Timeouts = config.Timeouts{Connect: 500 * time.Millisecond, Command: 200 * time.Millisecond, Scrape: 2 * time.Second}
	c.MetadataTTL = time.Hour
	return c
}

func newEnv(t *testing.T, cfg config.Config, servers ...named) *env {
	t.Helper()
	e := &env{t: t, cfg: cfg}
	var cols []export.Collector
	for _, s := range servers {
		addr := s.addr
		if s.f != nil {
			addr = s.f.srv.Addr()
		}
		c := collect.NewServer(collect.Options{
			Name: s.name, Address: addr, Workers: cfg.Workers, MetadataTTL: cfg.MetadataTTL,
			Client: nut.Options{ConnectTimeout: cfg.Timeouts.Connect, CommandTimeout: cfg.Timeouts.Command,
				MaxLine: cfg.Limits.MaxLine, MaxResponse: cfg.Limits.MaxResponse},
		})
		e.cols = append(e.cols, c)
		cols = append(cols, c)
	}
	exp, err := export.New(cfg, cols, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	e.exp = exp
	return e
}

// startMeta runs the metadata refreshers until the test ends.
func (e *env) startMeta() {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for _, c := range e.cols {
		wg.Add(1)
		go func() { defer wg.Done(); c.Run(ctx) }()
	}
	e.t.Cleanup(func() { cancel(); wg.Wait() })
}

// samples maps a canonical series key to its value.
type samples map[string]float64

// k builds the canonical key name{a="b",c="d"} with labels sorted by name.
func k(name string, kv ...string) string {
	var ls []string
	for i := 0; i+1 < len(kv); i += 2 {
		ls = append(ls, kv[i]+"="+q(kv[i+1]))
	}
	sort.Strings(ls)
	return name + "{" + strings.Join(ls, ",") + "}"
}

func parse(t *testing.T, r io.Reader) samples {
	t.Helper()
	p := expfmt.NewTextParser(model.UTF8Validation)
	fams, err := p.TextToMetricFamilies(r)
	if err != nil {
		t.Fatalf("parse exposition: %v", err)
	}
	out := samples{}
	for name, mf := range fams {
		for _, m := range mf.Metric {
			var kv []string
			for _, l := range m.Label {
				kv = append(kv, l.GetName(), l.GetValue())
			}
			var v float64
			switch {
			case m.Gauge != nil:
				v = m.Gauge.GetValue()
			case m.Counter != nil:
				v = m.Counter.GetValue()
			case m.Untyped != nil:
				v = m.Untyped.GetValue()
			}
			key := k(name, kv...)
			if _, dup := out[key]; dup {
				t.Fatalf("duplicate series %s", key)
			}
			out[key] = v
		}
	}
	return out
}

func (e *env) serve(ctx context.Context, path string, h func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", path, nil).WithContext(ctx))
	return rec
}

func (e *env) scrape() samples {
	e.t.Helper()
	rec := e.serve(context.Background(), "/metrics", e.exp.ServeMetrics)
	if rec.Code != 200 {
		e.t.Fatalf("/metrics status %d: %s", rec.Code, rec.Body.String())
	}
	return parse(e.t, rec.Body)
}

func (e *env) inventory() map[string]any {
	e.t.Helper()
	rec := e.serve(context.Background(), "/inventory", e.exp.ServeInventory)
	if rec.Code != 200 {
		e.t.Fatalf("/inventory status %d", rec.Code)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		e.t.Fatalf("inventory JSON: %v", err)
	}
	return m
}

// waitFor scrapes until cond holds, or fails at the deadline.
func (e *env) waitFor(d time.Duration, what string, cond func(samples) bool) samples {
	e.t.Helper()
	end := time.Now().Add(d)
	for {
		s := e.scrape()
		if cond(s) {
			return s
		}
		if time.Now().After(end) {
			e.t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// onlyFamilies keeps the series of the given families.
func onlyFamilies(s samples, names ...string) samples {
	out := samples{}
	for key, v := range s {
		for _, n := range names {
			if strings.HasPrefix(key, n+"{") {
				out[key] = v
			}
		}
	}
	return out
}

// withLabel keeps the series whose key contains the label pair.
func withLabel(s samples, name, value string) samples {
	out := samples{}
	needle := name + "=" + q(value)
	for key, v := range s {
		if strings.Contains(key, needle) {
			out[key] = v
		}
	}
	return out
}

func compare(t *testing.T, what string, got, want samples) {
	t.Helper()
	var msgs []string
	for key, w := range want {
		g, ok := got[key]
		switch {
		case !ok:
			msgs = append(msgs, "missing  "+key+" "+fmt.Sprint(w))
		case g != w:
			msgs = append(msgs, fmt.Sprintf("value    %s got %v want %v", key, g, w))
		}
	}
	for key, g := range got {
		if _, ok := want[key]; !ok {
			msgs = append(msgs, "extra    "+key+" "+fmt.Sprint(g))
		}
	}
	sort.Strings(msgs)
	if len(msgs) > 0 {
		t.Errorf("%s: %d differences:\n%s", what, len(msgs), strings.Join(msgs, "\n"))
	}
}

func expect(t *testing.T, s samples, key string, want float64) {
	t.Helper()
	g, ok := s[key]
	if !ok {
		t.Errorf("missing series %s", key)
	} else if g != want {
		t.Errorf("%s = %v, want %v", key, g, want)
	}
}

func absent(t *testing.T, s samples, key string) {
	t.Helper()
	if v, ok := s[key]; ok {
		t.Errorf("unexpected series %s = %v", key, v)
	}
}

func absentFamily(t *testing.T, s samples, labelName, labelValue string, families ...string) {
	t.Helper()
	for key, v := range onlyFamilies(withLabel(s, labelName, labelValue), families...) {
		t.Errorf("unexpected series %s = %v", key, v)
	}
}

func uk(name, srv, ups string, kv ...string) string {
	return k(name, append([]string{"server", srv, "ups", ups}, kv...)...)
}

// invUPS returns the inventory object of one UPS.
func invUPS(t *testing.T, inv map[string]any, server, ups string) map[string]any {
	t.Helper()
	for _, s := range inv["servers"].([]any) {
		sm := s.(map[string]any)
		if sm["name"] != server {
			continue
		}
		for _, u := range sm["ups"].([]any) {
			um := u.(map[string]any)
			if um["name"] == ups {
				return um
			}
		}
	}
	t.Fatalf("UPS %s/%s not in inventory", server, ups)
	return nil
}

func invVar(t *testing.T, u map[string]any, name string) map[string]any {
	t.Helper()
	for _, v := range u["variables"].([]any) {
		vm := v.(map[string]any)
		if vm["name"] == name {
			return vm
		}
	}
	t.Fatalf("variable %s not in inventory", name)
	return nil
}

func fakeErr(line string) fakeupsd.Response { return fakeupsd.Response{Lines: []string{line}} }
