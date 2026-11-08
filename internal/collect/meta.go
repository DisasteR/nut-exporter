package collect

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/DisasteR/nut-exporter/internal/nut"
)

// Metadata query names.
const (
	MetaType    = "type"
	MetaDesc    = "desc"
	MetaEnum    = "enum"
	MetaRange   = "range"
	MetaCmdDesc = "cmddesc"
)

var varMetaQueries = []string{MetaType, MetaDesc, MetaEnum, MetaRange}

const (
	retryMin  = time.Minute
	upsPause  = time.Minute
	idleWait  = time.Second
	dialPause = 30 * time.Second
)

type entry struct {
	MetaEntry
	next    time.Time // when the entry is due
	backoff time.Duration
}

type upsMeta struct {
	vars   map[string]map[string]*entry // variable -> query -> entry
	cmds   map[string]*entry
	paused time.Time
}

// metaCache holds metadata entries for listed items only (design §3).
type metaCache struct {
	mu  sync.Mutex
	ttl time.Duration
	ups map[string]*upsMeta
	rr  int
	now func() time.Time
}

func newMetaCache(ttl time.Duration) *metaCache {
	return &metaCache{ttl: ttl, ups: map[string]*upsMeta{}, now: time.Now}
}

func pending() *entry { return &entry{MetaEntry: MetaEntry{Query: Query{State: StatePending}}} }

// sync aligns the entries with a collection whose discovery succeeded: UPSes no longer collected
// are dropped; variables and commands follow their LIST answer when that query succeeded, and
// are kept as they are otherwise (a failed query proves no removal).
func (m *metaCache) sync(ups []UPS) {
	m.mu.Lock()
	defer m.mu.Unlock()
	keep := map[string]bool{}
	for _, u := range ups {
		if !u.Collected {
			continue
		}
		keep[u.Name] = true
		um := m.ups[u.Name]
		if um == nil {
			um = &upsMeta{vars: map[string]map[string]*entry{}, cmds: map[string]*entry{}}
			m.ups[u.Name] = um
		}
		if u.Queries[QueryVars].OK() {
			listed := map[string]bool{}
			for _, v := range u.Vars {
				listed[v.Name] = true
				if um.vars[v.Name] == nil {
					um.vars[v.Name] = map[string]*entry{}
					for _, q := range varMetaQueries {
						um.vars[v.Name][q] = pending()
					}
				}
			}
			for name := range um.vars {
				if !listed[name] {
					delete(um.vars, name)
				}
			}
		}
		if u.Queries[QueryCommands].OK() {
			listed := map[string]bool{}
			for _, c := range u.Commands {
				listed[c] = true
				if um.cmds[c] == nil {
					um.cmds[c] = pending()
				}
			}
			for name := range um.cmds {
				if !listed[name] {
					delete(um.cmds, name)
				}
			}
		}
	}
	for name := range m.ups {
		if !keep[name] {
			delete(m.ups, name)
		}
	}
}

// copy returns the current metadata of a UPS.
func (m *metaCache) copy(name string) (map[string]VarMeta, map[string]MetaEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	um := m.ups[name]
	if um == nil {
		return nil, nil
	}
	vars := make(map[string]VarMeta, len(um.vars))
	for v, qs := range um.vars {
		vars[v] = VarMeta{Type: qs[MetaType].clone(), Desc: qs[MetaDesc].clone(),
			Enum: qs[MetaEnum].clone(), Range: qs[MetaRange].clone()}
	}
	cmds := make(map[string]MetaEntry, len(um.cmds))
	for c, e := range um.cmds {
		cmds[c] = e.clone()
	}
	return vars, cmds
}

func (e *entry) clone() MetaEntry {
	c := e.MetaEntry
	c.Value = append([]string(nil), e.Value...)
	return c
}

type job struct {
	ups, item, query string
	e                *entry
}

// due picks the next due entry, rotating across UPSes one entry at a time.
func (m *metaCache) due() (job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	names := make([]string, 0, len(m.ups))
	for n := range m.ups {
		names = append(names, n)
	}
	sort.Strings(names)
	for i := range names {
		n := names[(m.rr+i)%len(names)]
		um := m.ups[n]
		if now.Before(um.paused) {
			continue
		}
		if j, ok := um.first(now); ok {
			m.rr = (m.rr + i + 1) % len(names)
			j.ups = n
			return j, true
		}
	}
	return job{}, false
}

// first returns the due entry of a UPS with the smallest (item, query).
func (um *upsMeta) first(now time.Time) (job, bool) {
	var best job
	found := false
	consider := func(item, query string, e *entry) {
		if now.Before(e.next) {
			return
		}
		if !found || item < best.item || (item == best.item && query < best.query) {
			best, found = job{item: item, query: query, e: e}, true
		}
	}
	for v, qs := range um.vars {
		for q, e := range qs {
			consider(v, q, e)
		}
	}
	for c, e := range um.cmds {
		consider(c, MetaCmdDesc, e)
	}
	return best, found
}

// commit stores a result if the entry is still the one listed (generation check by identity).
func (m *metaCache) commit(j job, value []string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	um := m.ups[j.ups]
	if um == nil {
		return
	}
	var cur *entry
	if j.query == MetaCmdDesc {
		cur = um.cmds[j.item]
	} else if qs := um.vars[j.item]; qs != nil {
		cur = qs[j.query]
	}
	if cur != j.e {
		return
	}
	now := m.now()
	cur.Query = result(err, now)
	if err == nil {
		cur.Value, cur.LastOK, cur.backoff = value, now, 0
		cur.next = now.Add(m.ttl)
		return
	}
	if cur.backoff == 0 {
		cur.backoff = retryMin
	} else if cur.backoff *= 2; cur.backoff > m.ttl {
		cur.backoff = m.ttl
	}
	cur.next = now.Add(cur.backoff)
	if cur.Error == "timeout" {
		um.paused = now.Add(upsPause)
	}
}

// Run refreshes metadata until ctx ends, on its own connection.
func (s *ServerCollector) Run(ctx context.Context) {
	var conn *nut.Conn
	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()
	for ctx.Err() == nil {
		j, ok := s.meta.due()
		if !ok {
			select {
			case <-ctx.Done():
			case <-time.After(idleWait):
			}
			continue
		}
		var err error
		if conn == nil || conn.Broken() {
			if conn, err = s.dial(ctx); err != nil {
				// Server unreachable: no entry is at fault, wait and retry the same work.
				conn = nil
				select {
				case <-ctx.Done():
				case <-time.After(dialPause):
				}
				continue
			}
		}
		v, err := fetch(ctx, conn, j)
		if ctx.Err() != nil {
			return
		}
		s.meta.commit(j, v, err)
	}
}

func fetch(ctx context.Context, c *nut.Conn, j job) ([]string, error) {
	switch j.query {
	case MetaType:
		return c.Type(ctx, j.ups, j.item)
	case MetaDesc:
		d, err := c.Description(ctx, j.ups, j.item)
		return []string{d}, err
	case MetaEnum:
		return c.ListEnum(ctx, j.ups, j.item)
	case MetaRange:
		rs, err := c.ListRange(ctx, j.ups, j.item)
		var out []string
		for _, r := range rs {
			out = append(out, r.Min, r.Max)
		}
		return out, err
	default:
		d, err := c.CommandDescription(ctx, j.ups, j.item)
		return []string{d}, err
	}
}
