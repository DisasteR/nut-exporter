package collect

import (
	"testing"
	"time"

	"github.com/DisasteR/nut-exporter/internal/nut"
)

func specListed(name string, vars ...string) UPS {
	u := UPS{Name: name, Collected: true, Queries: map[string]Query{QueryVars: {State: StateOK}, QueryCommands: {State: StateOK}}}
	for _, v := range vars {
		u.Vars = append(u.Vars, nut.Var{Name: v, Value: "1"})
	}
	return u
}

// Catches: a short TTL letting refreshed entries starve initially pending metadata.
func TestSpecMetadataPendingFairness(t *testing.T) {
	now := time.Unix(1000, 0)
	m := newMetaCache(time.Second)
	m.now = func() time.Time { return now }
	m.sync([]UPS{specListed("a", "x", "y"), specListed("b", "x", "y")})
	seen := map[[3]string]bool{}
	for i := 0; i < 16; i++ {
		j, ok := m.due()
		if !ok {
			t.Fatalf("no due entry at step %d", i)
		}
		key := [3]string{j.ups, j.item, j.query}
		if seen[key] {
			t.Fatalf("%v refreshed twice with %d of 16 initial entries visited", key, len(seen))
		}
		seen[key] = true
		m.commit(j, []string{"ok"}, nil)
		now = now.Add(2 * time.Second)
	}
}

// Catches: UPS rotation taking priority over an earlier due time.
func TestSpecMetadataOldestDueFirst(t *testing.T) {
	now := time.Unix(1000, 0)
	m := newMetaCache(time.Minute)
	m.now = func() time.Time { return now }
	a, b := specListed("a"), specListed("b")
	a.Commands = []string{"cmd"}
	b.Commands = []string{"cmd"}
	m.sync([]UPS{a, b})
	first, ok := m.due()
	if !ok || first.ups != "a" {
		t.Fatalf("first = %+v, %v", first, ok)
	}
	m.commit(first, []string{"a"}, nil)
	second, ok := m.due()
	if !ok || second.ups != "b" {
		t.Fatalf("second = %+v, %v", second, ok)
	}
	now = now.Add(10 * time.Second)
	m.commit(second, []string{"b"}, nil)
	now = now.Add(time.Minute)
	// Selecting a rotates the tie-breaker but does not change its earlier due time.
	j, ok := m.due()
	if !ok || j.ups != "a" {
		t.Fatalf("oldest = %+v, %v", j, ok)
	}
	j, ok = m.due()
	if !ok || j.ups != "a" {
		t.Fatalf("rotation hid still-oldest a: %+v, %v", j, ok)
	}
}

// Catches: unstable ties instead of UPS rotation followed by lexical item/query order.
func TestSpecMetadataDueTieOrder(t *testing.T) {
	now := time.Unix(1000, 0)
	m := newMetaCache(time.Hour)
	m.now = func() time.Time { return now }
	m.sync([]UPS{specListed("b", "z", "a"), specListed("a", "z", "a")})
	for _, item := range []string{"a", "z"} {
		for _, query := range []string{"desc", "enum", "range", "type"} {
			for _, ups := range []string{"a", "b"} {
				j, ok := m.due()
				if !ok || j.ups != ups || j.item != item || j.query != query {
					t.Fatalf("got %+v (%v), want %s/%s/%s", j, ok, ups, item, query)
				}
				m.commit(j, []string{"ok"}, nil)
			}
		}
	}
	if j, ok := m.due(); ok {
		t.Fatalf("not-yet-due entry returned: %+v", j)
	}
}

// Catches: an in-flight answer resurrecting a deleted item or contaminating its replacement.
func TestSpecMetadataDeletedInFlight(t *testing.T) {
	for _, readd := range []bool{false, true} {
		m := newMetaCache(time.Hour)
		m.sync([]UPS{specListed("a", "x")})
		j, ok := m.due()
		if !ok {
			t.Fatal("no pending job")
		}
		m.sync([]UPS{specListed("a")})
		if readd {
			m.sync([]UPS{specListed("a", "x")})
		}
		m.commit(j, []string{"stale answer"}, nil)
		vars, _ := m.copy("a")
		if !readd {
			if _, ok := vars["x"]; ok {
				t.Fatal("deleted item resurrected")
			}
			continue
		}
		v, ok := vars["x"]
		if !ok {
			t.Fatal("replacement missing")
		}
		for _, e := range []MetaEntry{v.Type, v.Desc, v.Enum, v.Range} {
			if e.State != StatePending || len(e.Value) != 0 || !e.LastOK.IsZero() {
				t.Fatalf("replacement contaminated: %+v", e)
			}
		}
	}
}
