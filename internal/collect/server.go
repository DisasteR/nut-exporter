package collect

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/DisasteR/nut-exporter/internal/nut"
)

// Options configures one server collector.
type Options struct {
	Name, Address string
	Client        nut.Options
	Workers       int
	MaxUPS        int
	MetadataTTL   time.Duration
	UPSFilter     func(name string) bool
}

// ServerCollector collects one upsd server. Collect may run concurrently with the metadata
// refresher, never with itself (the coordinator serialises collections).
type ServerCollector struct {
	opt  Options
	dial func(ctx context.Context) (*nut.Conn, error)
	meta *metaCache
}

// NewServer returns a collector; call Run to start its metadata refresher.
func NewServer(opt Options) *ServerCollector {
	if opt.Workers < 1 {
		opt.Workers = 1
	}
	s := &ServerCollector{opt: opt}
	s.dial = func(ctx context.Context) (*nut.Conn, error) { return nut.Dial(ctx, opt.Address, opt.Client) }
	s.meta = newMetaCache(opt.MetadataTTL)
	if opt.Client.MaxResponse > 0 {
		s.meta.budget = opt.Client.MaxResponse
	}
	return s
}

type item struct {
	ups   int // index in the UPS slice
	query string
}

// Collect runs the core collection (design §3) within ctx.
func (s *ServerCollector) Collect(ctx context.Context) (srv Server) {
	start := time.Now()
	srv = Server{Name: s.opt.Name, Address: s.opt.Address, Queries: map[string]Query{}}
	defer func() { srv.Duration = time.Since(start) }()

	conn, err := s.dial(ctx)
	if err != nil {
		srv.Queries[QueryDiscovery] = result(err, time.Now())
		return srv
	}
	list, err := conn.ListUPS(ctx)
	srv.Queries[QueryDiscovery] = result(err, time.Now())
	if err != nil {
		conn.Close()
		return srv
	}
	// Version queries are independent of UPS work: they run on their own connection meanwhile.
	var ver versions
	done := make(chan struct{})
	go func() { defer close(done); ver = s.versions(ctx) }()

	srv.UPS = s.selectUPS(list)
	s.runQueue(ctx, conn, srv.UPS)
	<-done
	srv.Version, srv.Protocol = ver.version, ver.protocol
	for q, st := range ver.queries {
		srv.Queries[q] = st
	}
	s.meta.sync(srv.UPS)
	for i := range srv.UPS {
		if srv.UPS[i].Collected {
			srv.UPS[i].VarMeta, srv.UPS[i].CmdMeta = s.meta.copy(srv.UPS[i].Name)
		}
	}
	return srv
}

// selectUPS resolves duplicate names (the last entry wins) before the UPS filter and max_ups,
// so a duplicate never consumes capacity (design §6).
func (s *ServerCollector) selectUPS(list []nut.UPS) []UPS {
	last := map[string]int{}
	for i, u := range list {
		last[u.Name] = i
	}
	var dups, kept []UPS
	for i, u := range list {
		x := UPS{Name: u.Name, Description: u.Description}
		if last[u.Name] != i {
			x.SkipReason = "duplicate"
			dups = append(dups, x)
			continue
		}
		kept = append(kept, x)
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].Name < kept[j].Name })
	collected := 0
	for i := range kept {
		x := &kept[i]
		switch {
		case s.opt.UPSFilter != nil && !s.opt.UPSFilter(x.Name):
			x.SkipReason = "filter"
		case s.opt.MaxUPS > 0 && collected >= s.opt.MaxUPS:
			x.SkipReason = "ups_cap"
		default:
			x.Collected = true
			x.Queries = map[string]Query{}
			collected++
		}
	}
	return append(kept, dups...)
}

type versions struct {
	version, protocol string
	queries           map[string]Query
}

// versions runs VER and NETVER on a connection of their own; a failure costs a reconnection.
func (s *ServerCollector) versions(ctx context.Context) versions {
	v := versions{queries: map[string]Query{}}
	var conn *nut.Conn
	run := func(name string, f func(*nut.Conn, context.Context) (string, error)) string {
		var err error
		if conn == nil || conn.Broken() {
			if conn, err = s.dial(ctx); err != nil {
				conn = nil
				v.queries[name] = result(err, time.Now())
				return ""
			}
		}
		out, err := f(conn, ctx)
		v.queries[name] = result(err, time.Now())
		return out
	}
	v.version = run(QueryVersion, (*nut.Conn).Version)
	v.protocol = run(QueryProtocol, (*nut.Conn).ProtocolVersion)
	if conn != nil {
		conn.Close()
	}
	return v
}

// runQueue executes the per-UPS queries: round-robin across UPSes, query-major, every
// LIST VAR first. A timed-out item moves its UPS's remaining items to the back of the queue.
func (s *ServerCollector) runQueue(ctx context.Context, first *nut.Conn, ups []UPS) {
	var (
		mu       sync.Mutex
		queue    []item
		deferred = map[int]bool{}
	)
	for _, q := range CoreQueries {
		for i := range ups {
			if ups[i].Collected {
				queue = append(queue, item{i, q})
				ups[i].Queries[q] = Query{State: StatePending}
			}
		}
	}
	next := func() (item, bool) {
		mu.Lock()
		defer mu.Unlock()
		if len(queue) == 0 {
			return item{}, false
		}
		it := queue[0]
		queue = queue[1:]
		return it, true
	}
	deferUPS := func(u int) {
		mu.Lock()
		defer mu.Unlock()
		if deferred[u] {
			return
		}
		deferred[u] = true
		var keep, moved []item
		for _, it := range queue {
			if it.ups == u {
				moved = append(moved, it)
			} else {
				keep = append(keep, it)
			}
		}
		queue = append(keep, moved...)
	}

	var wg sync.WaitGroup
	for w := 0; w < s.opt.Workers; w++ {
		var conn *nut.Conn
		if w == 0 && first != nil && !first.Broken() {
			conn = first
		}
		wg.Add(1)
		go func(conn *nut.Conn) {
			defer wg.Done()
			defer func() {
				if conn != nil {
					conn.Close()
				}
			}()
			for ctx.Err() == nil {
				it, ok := next()
				if !ok {
					return
				}
				var err error
				if conn == nil || conn.Broken() {
					if conn, err = s.dial(ctx); err != nil {
						conn = nil
						s.store(&mu, ups, it, err, nil)
						continue
					}
				}
				err = s.run(ctx, conn, &mu, ups, it)
				if err != nil && errorWord(err) == "timeout" && ctx.Err() == nil {
					deferUPS(it.ups)
				}
			}
		}(conn)
	}
	wg.Wait()
	// Items never started or interrupted by the deadline.
	for i := range ups {
		for q, st := range ups[i].Queries {
			if st.State == StatePending {
				ups[i].Queries[q] = Query{State: StateError, At: time.Now(), Error: "timeout"}
			}
		}
	}
}

// run executes one item and stores its result.
func (s *ServerCollector) run(ctx context.Context, c *nut.Conn, mu *sync.Mutex, ups []UPS, it item) error {
	name := ups[it.ups].Name
	var (
		err   error
		apply func(*UPS)
	)
	switch it.query {
	case QueryVars:
		var v []nut.Var
		v, err = c.ListVar(ctx, name)
		apply = func(u *UPS) { u.Vars = v }
	case QueryLogins:
		var n int
		n, err = c.NumLogins(ctx, name)
		apply = func(u *UPS) { u.Logins = n }
	case QueryClients:
		var v []string
		v, err = c.ListClient(ctx, name)
		apply = func(u *UPS) { u.Clients = v }
	case QueryCommands:
		var v []string
		v, err = c.ListCmd(ctx, name)
		apply = func(u *UPS) { u.Commands = v }
	case QueryRW:
		var v []nut.Var
		v, err = c.ListRW(ctx, name)
		apply = func(u *UPS) { u.RW = v }
	}
	s.store(mu, ups, it, err, apply)
	return err
}

func (s *ServerCollector) store(mu *sync.Mutex, ups []UPS, it item, err error, apply func(*UPS)) {
	mu.Lock()
	defer mu.Unlock()
	ups[it.ups].Queries[it.query] = result(err, time.Now())
	if err == nil && apply != nil {
		apply(&ups[it.ups])
	}
}
