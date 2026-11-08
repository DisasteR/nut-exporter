package export_test

import (
	"context"
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DisasteR/nut-exporter/internal/fakeupsd"
)

func common(name string) *upsSpec {
	u := newUPS(name, "battery.charge", "100", "device.model", "M-"+name, "ups.status", "OL")
	u.logins = 2
	u.clients = []string{"10.0.0.1", "10.0.0.1", "10.0.0.2"}
	u.cmds = []string{"beeper.enable"}
	return u
}

// Catches: series keyed by variable only (UPSes merged), by UPS name only (servers merged), a UPS list
// frozen at startup, and a registry that rejects duplicate series.
func TestDistinctSeriesAndNewUPS(t *testing.T) {
	a, b := common("a"), common("b")
	a.vars[0] = varLine("a", "battery.charge", "90")
	b.vars[0] = varLine("b", "battery.charge", "80")
	f1 := newFake(t, a, b)
	s2 := common("a")
	s2.vars[0] = varLine("a", "battery.charge", "70")
	f2 := newFake(t, s2)
	e := newEnv(t, baseCfg(), named{name: "s1", f: f1}, named{name: "s2", f: f2})
	got := e.scrape()
	expect(t, got, uk("nut_variable_value", "s1", "a", "variable", "battery.charge"), 90)
	expect(t, got, uk("nut_variable_value", "s1", "b", "variable", "battery.charge"), 80)
	expect(t, got, uk("nut_variable_value", "s2", "a", "variable", "battery.charge"), 70)
	expect(t, got, uk("nut_variable_info", "s1", "a", "variable", "device.model", "value", "M-a"), 1)
	expect(t, got, uk("nut_variable_info", "s1", "b", "variable", "device.model", "value", "M-b"), 1)
	expect(t, got, uk("nut_variable_info", "s2", "a", "variable", "device.model", "value", "M-a"), 1)
	expect(t, got, uk("nut_ups_client_sessions", "s1", "a", "address", "10.0.0.1"), 2)
	expect(t, got, uk("nut_ups_client_sessions", "s1", "a", "address", "10.0.0.2"), 1)
	expect(t, got, k("nut_server_up", "server", "s1"), 1)
	expect(t, got, k("nut_server_up", "server", "s2"), 1)
	absentFamily(t, got, "ups", "c", "nut_variable_value")

	f1.update(func() { f1.ups = append(f1.ups, common("c")) })
	got = e.scrape()
	expect(t, got, uk("nut_variable_value", "s1", "c", "variable", "battery.charge"), 100)
	expect(t, got, uk("nut_variable_value", "s1", "a", "variable", "battery.charge"), 90)
}

// Catches: a dead server hiding the others, and zeros exported for an unreachable server's UPSes (R6, R7).
func TestServerDownIsolated(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	good := newFake(t, common("a"))
	e := newEnv(t, baseCfg(), named{name: "dead", addr: addr}, named{name: "good", f: good})
	got := e.scrape()
	expect(t, got, k("nut_server_up", "server", "dead"), 0)
	expect(t, got, k("nut_server_up", "server", "good"), 1)
	absentFamily(t, got, "server", "dead", "nut_ups_up", "nut_ups_info", "nut_variable_value", "nut_variable_info")
	expect(t, got, uk("nut_variable_value", "good", "a", "variable", "battery.charge"), 100)
}

// Catches: ERR DATA-STALE on LIST VAR hiding clients/commands of that UPS, or the other UPS (R6, section 8).
func TestDataStaleIsolation(t *testing.T) {
	a, b := common("a"), common("b")
	a.override = map[string]fakeupsd.Response{
		"LIST VAR a":      {Lines: []string{"ERR DATA-STALE"}},
		"GET NUMLOGINS a": {Lines: []string{"ERR DATA-STALE"}},
	}
	f := newFake(t, a, b)
	e := newEnv(t, baseCfg(), named{name: "nas", f: f})
	got := e.scrape()
	expect(t, got, uk("nut_ups_info", "nas", "a", "description", "desc a"), 1)
	expect(t, got, uk("nut_ups_up", "nas", "a"), 0)
	expect(t, got, uk("nut_query_success", "nas", "a", "query", "vars"), 0)
	expect(t, got, uk("nut_query_error_info", "nas", "a", "query", "vars", "error", "DATA-STALE"), 1)
	expect(t, got, uk("nut_query_error_info", "nas", "a", "query", "logins", "error", "DATA-STALE"), 1)
	expect(t, got, uk("nut_ups_client_sessions", "nas", "a", "address", "10.0.0.1"), 2)
	expect(t, got, uk("nut_ups_command_info", "nas", "a", "command", "beeper.enable"), 1)
	expect(t, got, uk("nut_query_success", "nas", "a", "query", "clients"), 1)
	absentFamily(t, got, "ups", "a", "nut_variable_value", "nut_variable_info", "nut_ups_status", "nut_ups_logins")
	expect(t, got, uk("nut_ups_up", "nas", "b"), 1)
	expect(t, got, uk("nut_variable_value", "nas", "b", "variable", "battery.charge"), 100)
	expect(t, got, uk("nut_ups_logins", "nas", "b"), 2)
	expect(t, got, uk("nut_ups_status", "nas", "b", "flag", "OL"), 1)
	expect(t, got, uk("nut_ups_client_sessions", "nas", "b", "address", "10.0.0.2"), 1)
	if inv := e.inventory(); invUPS(t, inv, "nas", "a")["complete"] != false {
		t.Error("inventory complete must be false when LIST VAR failed")
	}
}

// Catches: sequential per-UPS collection (hanging UPSes consume the whole deadline), no deferral, and a
// shared deadline that exports nothing when two UPSes hang.
func TestHangingUPSesDoNotStarveHealthy(t *testing.T) {
	a, b, c := common("a"), common("b"), common("c")
	a.hangAll, b.hangAll = true, true
	f := newFake(t, a, b, c)
	e := newEnv(t, baseCfg(), named{name: "nas", f: f})
	start := time.Now()
	got := e.scrape()
	if d := time.Since(start); d > e.cfg.Timeouts.Scrape+time.Second {
		t.Errorf("scrape took %v, want <= scrape timeout %v + margin", d, e.cfg.Timeouts.Scrape)
	}
	for _, qn := range []string{"vars", "logins", "clients", "commands", "rw"} {
		expect(t, got, uk("nut_query_success", "nas", "c", "query", qn), 1)
		for _, h := range []string{"a", "b"} {
			expect(t, got, uk("nut_query_success", "nas", h, "query", qn), 0)
			expect(t, got, uk("nut_query_error_info", "nas", h, "query", qn, "error", "timeout"), 1)
		}
	}
	expect(t, got, uk("nut_ups_up", "nas", "c"), 1)
	expect(t, got, uk("nut_variable_value", "nas", "c", "variable", "battery.charge"), 100)
	expect(t, got, uk("nut_ups_logins", "nas", "c"), 2)
	expect(t, got, uk("nut_ups_client_sessions", "nas", "c", "address", "10.0.0.1"), 2)
	expect(t, got, uk("nut_ups_command_info", "nas", "c", "command", "beeper.enable"), 1)
	expect(t, got, uk("nut_ups_up", "nas", "a"), 0)
	expect(t, got, uk("nut_ups_info", "nas", "a", "description", "desc a"), 1)
}

// Catches: a LIST VAR timeout aborting the UPS's other queries (R6).
// Runs with one worker so the other queries of `a` are still queued when LIST VAR times out:
// with two workers the second one drains them first and deferral is never exercised.
// Catches: skipping (instead of deferring) a UPS's remaining queries after a timeout.
func TestOnlyListVarHangs(t *testing.T) {
	a, b := common("a"), common("b")
	a.override = map[string]fakeupsd.Response{"LIST VAR a": {Hang: true}}
	f := newFake(t, a, b)
	cfg := baseCfg()
	cfg.Workers = 1
	e := newEnv(t, cfg, named{name: "nas", f: f})
	got := e.scrape()
	expect(t, got, uk("nut_ups_up", "nas", "a"), 0)
	expect(t, got, uk("nut_query_error_info", "nas", "a", "query", "vars", "error", "timeout"), 1)
	expect(t, got, uk("nut_ups_logins", "nas", "a"), 2)
	expect(t, got, uk("nut_ups_client_sessions", "nas", "a", "address", "10.0.0.1"), 2)
	expect(t, got, uk("nut_ups_command_info", "nas", "a", "command", "beeper.enable"), 1)
	for _, qn := range []string{"logins", "clients", "commands", "rw"} {
		expect(t, got, uk("nut_query_success", "nas", "a", "query", qn), 1)
	}
	absentFamily(t, got, "ups", "a", "nut_variable_value", "nut_variable_info", "nut_ups_status")
	expect(t, got, uk("nut_variable_value", "nas", "b", "variable", "battery.charge"), 100)
}

// Catches: VER failure aborting the collection, or reported as success; absence of a version exported
// as a fabricated value.
func TestVersionFailure(t *testing.T) {
	for name, resp := range map[string]fakeupsd.Response{
		"err":  {Lines: []string{"ERR ACCESS-DENIED"}},
		"hang": {Hang: true},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t, common("a"))
			f.override["VER"] = resp
			e := newEnv(t, baseCfg(), named{name: "nas", f: f})
			got := e.scrape()
			expect(t, got, k("nut_server_up", "server", "nas"), 1)
			expect(t, got, k("nut_server_query_success", "server", "nas", "query", "discovery"), 1)
			expect(t, got, k("nut_server_query_success", "server", "nas", "query", "version"), 0)
			expect(t, got, k("nut_server_query_success", "server", "nas", "query", "protocol"), 1)
			expect(t, got, uk("nut_variable_value", "nas", "a", "variable", "battery.charge"), 100)
			expect(t, got, uk("nut_ups_logins", "nas", "a"), 2)
			// Design 3 and 5 disagree (info removed vs empty label); either way no invented version.
			for key := range onlyFamilies(got, "nut_server_info") {
				if !strings.Contains(key, `version=""`) {
					t.Errorf("nut_server_info with a version although VER failed: %s", key)
				}
			}
		})
	}
}

// Catches: unread status exported as zeros; repeated spaces/duplicates mishandled; an unknown token lost
// or never reset to 0; stale flags kept after LIST VAR fails.
func TestStatusFlags(t *testing.T) {
	u := newUPS("ups", "ups.status", "OL  CHRG OL")
	f := newFake(t, u)
	e := newEnv(t, baseCfg(), named{name: "nas", f: f})
	flag := func(s string) string { return uk("nut_ups_status", "nas", "ups", "flag", s) }
	known := strings.Fields("OL OB LB HB RB CHRG DISCHRG BYPASS CAL OFF OVER TRIM BOOST FSD ALARM WAIT")

	got := e.scrape()
	want := samples{}
	for _, fl := range known {
		want[flag(fl)] = 0
	}
	want[flag("OL")], want[flag("CHRG")] = 1, 1
	compare(t, "status 1", onlyFamilies(got, "nut_ups_status"), want)
	expect(t, got, uk("nut_variable_info", "nas", "ups", "variable", "ups.status", "value", "OL  CHRG OL"), 1)

	set := func(v string) { f.update(func() { u.vars = []string{varLine("ups", "ups.status", v)} }) }
	set("OL FOO")
	got = e.scrape()
	want[flag("OL")], want[flag("CHRG")], want[flag("FOO")] = 1, 0, 1
	compare(t, "status 2", onlyFamilies(got, "nut_ups_status"), want)

	set("OL")
	got = e.scrape()
	want[flag("FOO")] = 0
	compare(t, "status 3 (FOO disappears as 0)", onlyFamilies(got, "nut_ups_status"), want)

	f.update(func() { u.override = map[string]fakeupsd.Response{"LIST VAR ups": {Lines: []string{"ERR DATA-STALE"}}} })
	got = e.scrape()
	absentFamily(t, got, "ups", "ups", "nut_ups_status")
	absentFamily(t, got, "ups", "ups", "nut_variable_info")
	expect(t, got, uk("nut_ups_up", "nas", "ups"), 0)
}

// Catches: unbounded unknown status flags (design section 5, max 32 per UPS).
func TestStatusTokensCap(t *testing.T) {
	var toks []string
	for i := 1; i <= 33; i++ {
		toks = append(toks, "Z"+string(rune('A'+i/26))+string(rune('A'+i%26)))
	}
	f := newFake(t, newUPS("ups", "ups.status", "OL "+strings.Join(toks, " ")))
	e := newEnv(t, baseCfg(), named{name: "nas", f: f})
	got := e.scrape()
	unknown := 0
	for key := range onlyFamilies(got, "nut_ups_status") {
		if strings.Contains(key, `flag="Z`) {
			unknown++
		}
	}
	if unknown != 32 {
		t.Errorf("unknown flags projected = %d, want 32", unknown)
	}
	expect(t, got, uk("nut_suppressed", "nas", "ups", "family", "nut_ups_status", "reason", "status_tokens"), 1)
	// The raw status is kept whole.
	raw := "OL " + strings.Join(toks, " ")
	expect(t, got, uk("nut_variable_info", "nas", "ups", "variable", "ups.status", "value", raw), 1)
}

// Catches: numeric guessing beyond the NUT grammar, value rewriting, opaque identifiers turned into gauges,
// bad escape handling, empty values dropped, invalid dates producing a timestamp (design section 4).
func TestClassificationGrammarDatesEscapes(t *testing.T) {
	u := newUPS("ups",
		"experimental.n1", "01200.2", "experimental.n2", "1200.20", "experimental.n3", "+5",
		"experimental.n4", "1e3", "experimental.n5", ".5", "experimental.n6", "5.",
		"experimental.n7", "-0.5", "experimental.n8", "NaN", "experimental.n9", "", "experimental.n10", " 5",
		"ups.serial", "123456", "ups.id", "42",
		"battery.date", "2024/02/29", "battery.mfr.date", "2023/02/29", "ups.mfr.date", "2026-99-99",
		"ups.test.date", "2025-02-28", "input.date", "2026/11-08", "ups.date.last", "11/07/26",
		"ups.date.start", "2000/02/29",
		"device.contact", "a \"q\" b\\c", "device.location", "")
	f := newFake(t, u)
	e := newEnv(t, baseCfg(), named{name: "nas", f: f})
	got := e.scrape()
	val := func(n string) string { return uk("nut_variable_value", "nas", "ups", "variable", n) }
	info := func(n, v string) string { return uk("nut_variable_info", "nas", "ups", "variable", n, "value", v) }
	expect(t, got, val("experimental.n1"), 1200.2)
	expect(t, got, val("experimental.n2"), 1200.2)
	expect(t, got, val("experimental.n7"), -0.5)
	for n, v := range map[string]string{"n3": "+5", "n4": "1e3", "n5": ".5", "n6": "5.", "n8": "NaN", "n9": "", "n10": " 5"} {
		expect(t, got, info("experimental."+n, v), 1)
		absent(t, got, val("experimental."+n))
	}
	expect(t, got, info("ups.serial", "123456"), 1)
	expect(t, got, info("ups.id", "42"), 1)
	absent(t, got, val("ups.serial"))
	expect(t, got, info("device.contact", "a \"q\" b\\c"), 1)
	expect(t, got, info("device.location", ""), 1)
	ts := func(n string) string { return uk("nut_variable_timestamp_seconds", "nas", "ups", "variable", n) }
	compare(t, "timestamps", onlyFamilies(got, "nut_variable_timestamp_seconds"), samples{
		ts("battery.date"): 1709164800, ts("ups.test.date"): 1740700800, ts("ups.date.start"): 951782400,
	})
	for _, n := range []string{"battery.mfr.date", "ups.mfr.date", "input.date", "ups.date.last"} {
		expect(t, got, info(n, map[string]string{"battery.mfr.date": "2023/02/29", "ups.mfr.date": "2026-99-99",
			"input.date": "2026/11-08", "ups.date.last": "11/07/26"}[n]), 1)
	}
}

// Catches: an HTTP request context cancelling the shared collection (design section 3), and one
// collection per caller.
func TestOverlappingScrapesOneCancelled(t *testing.T) {
	u := common("ups")
	u.delay = map[string]time.Duration{"LIST VAR ups": 500 * time.Millisecond}
	f := newFake(t, u)
	cfg := baseCfg()
	cfg.Timeouts.Command = time.Second // the slow answer must succeed
	e := newEnv(t, cfg, named{name: "nas", f: f})
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	var wg sync.WaitGroup
	var recA, recB *httptest.ResponseRecorder
	wg.Add(2)
	go func() { defer wg.Done(); recA = e.serve(ctxA, "/metrics", e.exp.ServeMetrics) }()
	time.Sleep(30 * time.Millisecond)
	go func() { defer wg.Done(); recB = e.serve(context.Background(), "/metrics", e.exp.ServeMetrics) }()
	time.Sleep(100 * time.Millisecond)
	cancelA()
	wg.Wait()
	if recB.Code != http.StatusOK {
		t.Fatalf("surviving scrape status %d", recB.Code)
	}
	got := parse(t, recB.Body)
	expect(t, got, uk("nut_ups_up", "nas", "ups"), 1)
	expect(t, got, uk("nut_variable_value", "nas", "ups", "variable", "battery.charge"), 100)
	expect(t, got, uk("nut_ups_logins", "nas", "ups"), 2)
	for _, qn := range []string{"vars", "logins", "clients", "commands", "rw"} {
		expect(t, got, uk("nut_query_success", "nas", "ups", "query", qn), 1)
	}
	if n := f.count("LIST UPS"); n != 1 {
		t.Errorf("LIST UPS sent %d times for two overlapping scrapes, want 1 (shared collection)", n)
	}
	_ = recA
}

// Catches: inventory built from successes only, `complete` true despite an error, wrong schema, and
// complete=true before metadata is known.
func TestInventoryCompleteAndSchema(t *testing.T) {
	a, b := common("a"), common("b")
	a.override = map[string]fakeupsd.Response{"LIST CLIENT a": {Lines: []string{"ERR ACCESS-DENIED"}}}
	f := newFake(t, a, b)
	e := newEnv(t, baseCfg(), named{name: "nas", f: f})
	inv := e.inventory()
	if inv["schema"] != float64(1) {
		t.Errorf("schema = %v, want 1", inv["schema"])
	}
	ua := invUPS(t, inv, "nas", "a")
	if ua["complete"] != false {
		t.Error("complete must be false when LIST CLIENT failed")
	}
	qc := ua["queries"].(map[string]any)["clients"].(map[string]any)
	if qc["state"] != "error" || qc["error"] != "ACCESS-DENIED" {
		t.Errorf("clients query = %v", qc)
	}
	if invUPS(t, inv, "nas", "b")["complete"] != false {
		t.Error("complete must be false while metadata entries are still pending (design 6b)")
	}
	// The core data of a is still there.
	if len(ua["variables"].([]any)) != 3 {
		t.Errorf("variables of a = %v", ua["variables"])
	}
}

// Catches: a non-UTF-8 value corrupted, dropped from the inventory, or exported as a label (design 6, 6b).
func TestEncodingGuardAndInventoryBase64(t *testing.T) {
	raw := "caf\xe9"
	u := newUPS("ups", "device.contact", raw, "device.model", "ok")
	f := newFake(t, u)
	e := newEnv(t, baseCfg(), named{name: "nas", f: f})
	got := e.scrape()
	absentFamily(t, got, "variable", "device.contact", "nut_variable_info")
	expect(t, got, uk("nut_suppressed", "nas", "ups", "family", "nut_variable_info", "reason", "encoding"), 1)
	expect(t, got, uk("nut_variable_info", "nas", "ups", "variable", "device.model", "value", "ok"), 1)
	v := invVar(t, invUPS(t, e.inventory(), "nas", "ups"), "device.contact")["value"]
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("inventory value = %#v, want {\"base64\": ...}", v)
	}
	dec, err := base64.StdEncoding.DecodeString(m["base64"].(string))
	if err != nil || string(dec) != raw {
		t.Errorf("base64 round trip = %q, %v; want %q", dec, err, raw)
	}
}
