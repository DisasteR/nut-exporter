// Copyright 2026 Benjamin Saiz
// SPDX-License-Identifier: Apache-2.0

package export

import (
	"hash/fnv"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/DisasteR/nut-exporter/internal/classify"
	"github.com/DisasteR/nut-exporter/internal/collect"
	"github.com/DisasteR/nut-exporter/internal/config"
)

// sample is one series of a scrape.
type sample struct {
	f      *family
	labels []string // all label values, scope labels first
	value  float64
	group  string // drop group under max_series: variable name for prioVariables, family name otherwise
}

// knownFlags are the documented ups.status flags (design §2).
var knownFlags = []string{"OL", "OB", "LB", "HB", "RB", "CHRG", "DISCHRG", "BYPASS", "CAL", "OFF",
	"OVER", "TRIM", "BOOST", "FSD", "ALARM", "WAIT"}

const maxUnknownFlags = 32

// state is what the projection keeps between scrapes. Guarded by mu: projections are serialised
// by the coordinator, the lock only protects against misuse.
type state struct {
	mu          sync.Mutex
	churn       map[string]*churnSet // server\x00ups\x00family\x00key
	unknown     map[string][]string  // server\x00ups -> remembered unknown status flags
	parseErrors map[string]float64   // server\x00ups\x00variable
}

type churnSet struct {
	seen     map[uint64]time.Time // label-set hash -> last seen
	lastUsed time.Time
}

func newState() *state {
	return &state{churn: map[string]*churnSet{}, unknown: map[string][]string{}, parseErrors: map[string]float64{}}
}

// projector turns one collection into samples.
type projector struct {
	cfg    config.Config
	cls    *classify.Classifier
	st     *state
	now    time.Time
	varSup map[string]string // server\x00ups\x00variable -> reason, for the inventory
}

func (p *projector) noteVar(sc []string, name, reason string) {
	if p.varSup != nil {
		p.varSup[key(sc[0], sc[1], name)] = reason
	}
}

func key(parts ...string) string { return strings.Join(parts, "\x00") }

func (p *projector) project(servers []collect.Server) []sample {
	p.st.mu.Lock()
	defer p.st.mu.Unlock()
	var out []sample
	for _, srv := range servers {
		out = append(out, p.server(srv)...)
	}
	p.evict(servers)
	return out
}

func (p *projector) server(srv collect.Server) []sample {
	s := []string{srv.Name}
	out := []sample{{f: fDuration, labels: s, value: srv.Duration.Seconds()}}
	disc := srv.Queries[collect.QueryDiscovery]
	out = append(out, sample{f: fServerUp, labels: s, value: b2f(disc.OK())})
	for _, q := range []string{collect.QueryDiscovery, collect.QueryVersion, collect.QueryProtocol} {
		if st, ok := srv.Queries[q]; ok {
			out = append(out, sample{f: fServerQuery, labels: with(s, q), value: b2f(st.OK())})
		}
	}
	// A label is empty when its query failed or its answer is not a valid label (raw in /inventory).
	ver, proto := srv.Version, srv.Protocol
	if !validLabel(ver, p.cfg.Guard.MaxLength) {
		ver = ""
	}
	if !validLabel(proto, p.cfg.Guard.MaxLength) {
		proto = ""
	}
	out = append(out, sample{f: fServerInfo, labels: with(s, ver, proto), value: 1})
	if !disc.OK() {
		return out
	}
	skipped := map[string]float64{"filter": 0, "ups_cap": 0, "encoding": 0, "duplicate": 0}
	for _, u := range srv.UPS {
		if !u.Collected {
			skipped[u.SkipReason]++
			continue
		}
		if !utf8.ValidString(u.Name) {
			skipped["encoding"]++
			continue
		}
		out = append(out, p.ups(srv.Name, u)...)
	}
	for _, r := range []string{"filter", "ups_cap", "encoding", "duplicate"} {
		out = append(out, sample{f: fServerSuppressed, labels: with(s, r), value: skipped[r]})
	}
	return out
}

// suppressions counts series not exported, per family and reason.
type suppressions map[[2]string]float64

func (sp suppressions) add(f *family, reason string, n int) {
	sp[[2]string{f.name, reason}] += float64(n)
}

func (p *projector) ups(server string, u collect.UPS) []sample {
	sc := []string{server, u.Name}
	sup := suppressions{}
	var health, budget []sample

	if validLabel(u.Description, p.cfg.Guard.MaxLength) {
		health = append(health, sample{f: fUPSInfo, labels: with(sc, u.Description), value: 1})
	} else {
		sup.add(fUPSInfo, labelReason(u.Description), 1)
	}
	health = append(health, sample{f: fUPSUp, labels: sc, value: b2f(u.Queries[collect.QueryVars].OK())})
	for _, q := range collect.CoreQueries {
		st := u.Queries[q]
		health = append(health, sample{f: fQuery, labels: with(sc, q), value: b2f(st.OK())})
		if !st.OK() && st.Error != "" {
			word := st.Error
			if !validLabel(word, 64) {
				word = "protocol"
			}
			health = append(health, sample{f: fQueryError, labels: with(sc, q, word), value: 1})
		}
	}
	health = append(health, p.metaHealth(sc, u)...)

	if u.Queries[collect.QueryVars].OK() {
		budget = append(budget, p.variables(sc, u, sup)...)
	}
	if u.Queries[collect.QueryLogins].OK() {
		health = append(health, sample{f: fLogins, labels: sc, value: float64(u.Logins)})
	}
	if u.Queries[collect.QueryClients].OK() {
		count := map[string]int{}
		for _, a := range u.Clients {
			count[a]++
		}
		var cs []sample
		for _, a := range sortedKeys(count) {
			cs = append(cs, sample{f: fClients, labels: with(sc, a), value: float64(count[a]), group: fClients.name})
		}
		budget = append(budget, p.guard(sc, fClients, "", cs, sup)...)
	}
	if u.Queries[collect.QueryCommands].OK() {
		var cs []sample
		for _, c := range sortedUnique(u.Commands) {
			cs = append(cs, sample{f: fCommands, labels: with(sc, c), value: 1, group: fCommands.name})
		}
		budget = append(budget, p.labels(sc, fCommands, cs, sup)...)
	}
	if u.Queries[collect.QueryRW].OK() {
		var ws []sample
		count := map[string]int{}
		for _, v := range u.RW {
			count[v.Name]++
		}
		for _, n := range sortedKeys(count) {
			if count[n] > 1 {
				sup.add(fWritable, "duplicate", count[n]-1)
			}
			if !p.cfg.Variables.Match(n) {
				sup.add(fWritable, "filter", 1)
				continue
			}
			ws = append(ws, sample{f: fWritable, labels: with(sc, n), value: 1, group: fWritable.name})
		}
		budget = append(budget, p.labels(sc, fWritable, ws, sup)...)
	}
	budget = append(budget, p.metadata(sc, u, sup)...)

	budget = p.capSeries(sc, budget, sup)
	for _, k := range sortedPairs(sup) {
		health = append(health, sample{f: fSuppressed, labels: with(sc, k[0], k[1]), value: sup[k]})
	}
	// Parse error counters are kept for every variable that ever failed, while the UPS exists.
	prefix := key(server, u.Name, "")
	for _, k := range sortedKeys(p.st.parseErrors) {
		if strings.HasPrefix(k, prefix) {
			health = append(health, sample{f: fParseErrors, labels: with(sc, strings.TrimPrefix(k, prefix)), value: p.st.parseErrors[k]})
		}
	}
	return append(health, budget...)
}

func (p *projector) variables(sc []string, u collect.UPS, sup suppressions) []sample {
	var out []sample
	// A repeated name in LIST VAR would give duplicate series: the last value wins, the others
	// are counted (design §6, reason duplicate).
	last := map[string]int{}
	for i, v := range u.Vars {
		last[v.Name] = i
	}
	listed := map[string]bool{}
	for i, v := range u.Vars {
		kind, num, parseErr := p.cls.Classify(v.Name, v.Value)
		fam, gaugeLabels := fInfo, []string(nil)
		if kind == classify.Gauge {
			fam, gaugeLabels = gaugeSample(sc, v.Name)
		}
		if last[v.Name] != i {
			sup.add(fam, "duplicate", 1)
			continue
		}
		listed[v.Name] = true
		if !p.cfg.Variables.Match(v.Name) {
			sup.add(fam, "filter", 1)
			p.noteVar(sc, v.Name, "filter")
			continue
		}
		if !validLabel(v.Name, p.cfg.Guard.MaxLength) {
			sup.add(fam, labelReason(v.Name), 1)
			p.noteVar(sc, v.Name, labelReason(v.Name))
			continue
		}
		if v.Name == "ups.status" {
			out = append(out, p.status(sc, v.Value, sup)...)
		}
		if parseErr {
			p.st.parseErrors[key(sc[0], sc[1], v.Name)]++
		}
		if kind == classify.Gauge {
			out = append(out, sample{f: fam, labels: gaugeLabels, value: num, group: v.Name})
			continue
		}
		info := sample{f: fInfo, labels: with(sc, v.Name, v.Value), value: 1, group: v.Name}
		kept, reason := p.guardReason(sc, fInfo, v.Name, []sample{info}, sup)
		if reason != "" {
			p.noteVar(sc, v.Name, reason)
		}
		out = append(out, kept...)
		if ts, ok := classify.Timestamp(v.Name, v.Value); ok {
			out = append(out, sample{f: fTimestamp, labels: with(sc, v.Name), value: float64(ts), group: fTimestamp.name})
		}
	}
	// Parse error counters follow the variable: dropped once it is no longer listed.
	prefix := key(sc[0], sc[1], "")
	for k := range p.st.parseErrors {
		if strings.HasPrefix(k, prefix) && !listed[strings.TrimPrefix(k, prefix)] {
			delete(p.st.parseErrors, k)
		}
	}
	return out
}

// status projects ups.status into flags: known flags always, unknown ones remembered (max 32).
func (p *projector) status(sc []string, raw string, sup suppressions) []sample {
	set := map[string]bool{}
	for _, f := range strings.Fields(raw) {
		set[f] = true
	}
	uk := key(sc[0], sc[1])
	remembered := p.st.unknown[uk]
	isKnown := map[string]bool{}
	for _, f := range knownFlags {
		isKnown[f] = true
	}
	for _, f := range sortedKeys(set) {
		if isKnown[f] || contains(remembered, f) {
			continue
		}
		if len(remembered) >= maxUnknownFlags || !validLabel(f, p.cfg.Guard.MaxLength) {
			sup.add(fStatus, "status_tokens", 1)
			delete(set, f)
			continue
		}
		remembered = append(remembered, f)
	}
	p.st.unknown[uk] = remembered
	var out []sample
	for _, f := range append(append([]string(nil), knownFlags...), remembered...) {
		out = append(out, sample{f: fStatus, labels: with(sc, f), value: b2f(set[f]), group: "ups.status"})
	}
	return out
}

// metadata projects the metadata cache. It depends on the cache only, not on the current LIST VAR:
// the cache keeps its entries while LIST VAR fails (design §5).
func (p *projector) metadata(sc []string, u collect.UPS, sup suppressions) []sample {
	var out []sample
	for _, name := range sortedKeys(u.VarMeta) {
		m := u.VarMeta[name]
		var ts, es, rs []sample
		if !m.Type.LastOK.IsZero() {
			for _, t := range sortedUnique(m.Type.Value) {
				ts = append(ts, sample{f: fType, labels: with(sc, name, t), value: 1, group: fType.name})
			}
		}
		if !m.Enum.LastOK.IsZero() {
			for _, e := range sortedUnique(m.Enum.Value) {
				es = append(es, sample{f: fEnum, labels: with(sc, name, e), value: 1, group: fEnum.name})
			}
		}
		if !m.Range.LastOK.IsZero() {
			seen := map[[2]string]bool{}
			for i := 0; i+1 < len(m.Range.Value); i += 2 {
				r := [2]string{m.Range.Value[i], m.Range.Value[i+1]}
				if !seen[r] {
					seen[r] = true
					rs = append(rs, sample{f: fRange, labels: with(sc, name, r[0], r[1]), value: 1, group: fRange.name})
				}
			}
		}
		if !p.cfg.Variables.Match(name) {
			for f, n := range map[*family]int{fType: len(ts), fEnum: len(es), fRange: len(rs)} {
				if n > 0 {
					sup.add(f, "filter", n)
				}
			}
			continue
		}
		out = append(out, p.guard(sc, fType, name, ts, sup)...)
		out = append(out, p.guard(sc, fEnum, name, es, sup)...)
		out = append(out, p.guard(sc, fRange, name, rs, sup)...)
	}
	return out
}

func (p *projector) metaHealth(sc []string, u collect.UPS) []sample {
	type agg struct {
		oldest          time.Time
		errors, pending int
		any             bool
	}
	aggs := map[string]*agg{}
	add := func(q string, e collect.MetaEntry) {
		a := aggs[q]
		if a == nil {
			a = &agg{}
			aggs[q] = a
		}
		a.any = true
		switch e.State {
		case collect.StatePending:
			a.pending++
		case collect.StateError:
			a.errors++
		}
		if !e.LastOK.IsZero() && (a.oldest.IsZero() || e.LastOK.Before(a.oldest)) {
			a.oldest = e.LastOK
		}
	}
	for _, m := range u.VarMeta {
		add(collect.MetaType, m.Type)
		add(collect.MetaDesc, m.Desc)
		add(collect.MetaEnum, m.Enum)
		add(collect.MetaRange, m.Range)
	}
	for _, e := range u.CmdMeta {
		add(collect.MetaCmdDesc, e)
	}
	var out []sample
	for _, q := range sortedKeys(aggs) {
		a := aggs[q]
		out = append(out, sample{f: fMetaErrors, labels: with(sc, q), value: float64(a.errors)},
			sample{f: fMetaPending, labels: with(sc, q), value: float64(a.pending)})
		if !a.oldest.IsZero() {
			out = append(out, sample{f: fMetaAge, labels: with(sc, q), value: p.now.Sub(a.oldest).Seconds()})
		}
	}
	return out
}

// labels applies the label rules (encoding, length) of design §6.
func (p *projector) labels(sc []string, f *family, in []sample, sup suppressions) []sample {
	ok, _ := p.labelReason(sc, f, in, sup)
	return ok
}

func (p *projector) labelReason(sc []string, f *family, in []sample, sup suppressions) ([]sample, string) {
	var ok []sample
	last := ""
	for _, s := range in {
		reason := ""
		for _, l := range s.labels[len(sc):] {
			if !validLabel(l, p.cfg.Guard.MaxLength) {
				reason = labelReason(l)
				break
			}
		}
		if reason != "" {
			sup.add(f, reason, 1)
			last = reason
			continue
		}
		ok = append(ok, s)
	}
	return ok, last
}

// guard applies the label rules and the churn rule of one (family, key) to its series.
func (p *projector) guard(sc []string, f *family, k string, in []sample, sup suppressions) []sample {
	ok, _ := p.guardReason(sc, f, k, in, sup)
	return ok
}

// guardReason is guard returning the reason when it suppressed something. Churn is turnover:
// the key is suppressed when the label sets seen within the window but absent now reach
// max_distinct (design §6); the size of a family alone never triggers it.
func (p *projector) guardReason(sc []string, f *family, k string, in []sample, sup suppressions) ([]sample, string) {
	ok, reason := p.labelReason(sc, f, in, sup)
	if len(ok) == 0 {
		return nil, reason
	}
	ck := key(sc[0], sc[1], f.name, k)
	cs := p.st.churn[ck]
	if cs == nil {
		cs = &churnSet{seen: map[uint64]time.Time{}}
		p.st.churn[ck] = cs
	}
	cs.lastUsed = p.now
	for h, t := range cs.seen {
		if p.now.Sub(t) > p.cfg.Guard.ChurnWindow {
			delete(cs.seen, h)
		}
	}
	current := map[uint64]bool{}
	for _, s := range ok {
		current[hashLabels(s.labels[len(sc):])] = true
	}
	for h := range current {
		if _, seen := cs.seen[h]; seen || len(cs.seen) < len(current)+p.cfg.Guard.MaxDistinct {
			cs.seen[h] = p.now
		}
	}
	gone := 0
	for h := range cs.seen {
		if !current[h] {
			gone++
		}
	}
	if gone >= p.cfg.Guard.MaxDistinct {
		sup.add(f, "churn", len(ok))
		return nil, "churn"
	}
	return ok, reason
}

// capSeries drops whole groups until the UPS fits max_series (design §6).
func (p *projector) capSeries(sc []string, in []sample, sup suppressions) []sample {
	if len(in) <= p.cfg.Guard.MaxSeries {
		return in
	}
	type grp struct {
		prio int
		name string
		n    int
	}
	groups := map[[2]string]*grp{}
	for _, s := range in {
		g := groups[[2]string{s.f.name, s.group}]
		if g == nil {
			name := s.group
			g = &grp{prio: s.f.budget, name: name}
			groups[[2]string{s.f.name, s.group}] = g
		}
		g.n++
	}
	// Variables (value, info, status) of one name form one group across families.
	byName := map[string]*grp{}
	var order []*grp
	for _, g := range groups {
		if g.prio == prioVariables {
			v := byName[g.name]
			if v == nil {
				v = &grp{prio: prioVariables, name: g.name}
				byName[g.name] = v
				order = append(order, v)
			}
			v.n += g.n
			continue
		}
		order = append(order, g)
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].prio != order[j].prio {
			return order[i].prio < order[j].prio
		}
		if order[i].prio == prioVariables {
			return order[i].name > order[j].name // reverse name order
		}
		return order[i].name < order[j].name
	})
	drop := map[string]bool{} // group key: prio|name
	total := len(in)
	for _, g := range order {
		if total <= p.cfg.Guard.MaxSeries {
			break
		}
		drop[gkey(g.prio, g.name)] = true
		total -= g.n
	}
	var out []sample
	for _, s := range in {
		if drop[gkey(s.f.budget, s.group)] {
			sup.add(s.f, "series_cap", 1)
			if s.f.budget == prioVariables {
				p.noteVar(sc, s.group, "series_cap")
			}
			continue
		}
		out = append(out, s)
	}
	return out
}

func gkey(prio int, name string) string { return string(rune('0'+prio)) + "|" + name }

// evict drops retained state of UPSes and keys no longer present (design §6).
func (p *projector) evict(servers []collect.Server) {
	live := map[string]bool{}
	discovered := map[string]bool{}
	listed := map[string]map[string]bool{} // UPS key -> variables of a successful LIST VAR
	for _, srv := range servers {
		if !srv.Queries[collect.QueryDiscovery].OK() {
			continue
		}
		discovered[srv.Name] = true
		for _, u := range srv.UPS {
			if !u.Collected {
				continue
			}
			uk := key(srv.Name, u.Name)
			live[uk] = true
			if u.Queries[collect.QueryVars].OK() {
				listed[uk] = map[string]bool{}
				for _, v := range u.Vars {
					listed[uk][v.Name] = true
				}
			}
		}
	}
	upsOf := func(k string) (string, string) {
		parts := strings.SplitN(k, "\x00", 3)
		return parts[0], key(parts[0], parts[1])
	}
	perVariable := map[string]bool{fInfo.name: true, fType.name: true, fEnum.name: true, fRange.name: true}
	for k, cs := range p.st.churn {
		srv, uk := upsOf(k)
		parts := strings.SplitN(k, "\x00", 4) // server, ups, family, variable
		gone := listed[uk] != nil && perVariable[parts[2]] && !listed[uk][parts[3]]
		if (discovered[srv] && !live[uk]) || gone || p.now.Sub(cs.lastUsed) > p.cfg.Guard.ChurnWindow {
			delete(p.st.churn, k)
		}
	}
	for uk := range p.st.unknown {
		if srv, _ := upsOf(uk); discovered[srv] && !live[uk] {
			delete(p.st.unknown, uk)
		}
	}
	for k := range p.st.parseErrors {
		if srv, uk := upsOf(k); discovered[srv] && !live[uk] {
			delete(p.st.parseErrors, k)
		}
	}
}

func labelReason(v string) string {
	if !utf8.ValidString(v) {
		return "encoding"
	}
	return "length"
}

func validLabel(v string, max int) bool { return utf8.ValidString(v) && len(v) <= max }

func hashLabels(ls []string) uint64 {
	h := fnv.New64a()
	for _, l := range ls {
		h.Write([]byte(l))
		h.Write([]byte{0})
	}
	return h.Sum64()
}

func with(base []string, more ...string) []string {
	return append(append(make([]string, 0, len(base)+len(more)), base...), more...)
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedUnique(l []string) []string {
	m := map[string]bool{}
	for _, x := range l {
		m[x] = true
	}
	return sortedKeys(m)
}

func sortedPairs(m suppressions) [][2]string {
	out := make([][2]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i][0] != out[j][0] {
			return out[i][0] < out[j][0]
		}
		return out[i][1] < out[j][1]
	})
	return out
}
