package export_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DisasteR/nut-exporter/internal/fakeupsd"
)

// Catches: retained metadata exceeding the per-UPS budget, or replacing a last-good value on overflow.
func TestSpecMetadataRetainedBudget(t *testing.T) {
	u := newUPS("a", "x", "1")
	u.typ = map[string]string{"x": "NUMBER"}
	u.descs = map[string]string{"x": strings.Repeat("d", 300)}
	old := strings.Repeat("e", 206) // 300 description + 6 type + 206 enum = 512 bytes exactly.
	u.enum = map[string][]string{"x": {old}}
	f := newFake(t, u)
	cfg := baseCfg()
	cfg.Limits.MaxResponse = 512
	cfg.MetadataTTL = time.Second
	e := newEnv(t, cfg, named{name: "s", f: f})
	e.scrape()
	e.startMeta()
	got := e.waitFor(10*time.Second, "initial metadata at exact budget", func(s samples) bool { return pendingZero(s, "a") })
	expect(t, got, uk("nut_variable_enum_info", "s", "a", "variable", "x", "value", old), 1)
	expect(t, got, uk("nut_metadata_errors", "s", "a", "query", "enum"), 0)
	f.update(func() { u.enum["x"] = []string{strings.Repeat("e", 207)} })
	got = e.waitFor(10*time.Second, "retained metadata budget rejection", func(s samples) bool { return s[uk("nut_metadata_errors", "s", "a", "query", "enum")] > 0 })
	expect(t, got, uk("nut_variable_enum_info", "s", "a", "variable", "x", "value", old), 1)
	absent(t, got, uk("nut_variable_enum_info", "s", "a", "variable", "x", "value", strings.Repeat("e", 207)))
	md := invVar(t, invUPS(t, e.inventory(), "s", "a"), "x")["metadata"].(map[string]any)
	enum := md["enum"].(map[string]any)
	if enum["state"] != "error" || enum["error"] != "too_large" || !reflect.DeepEqual(enum["value"], []any{old}) {
		t.Errorf("rejected enum = %v", enum)
	}
	for query, want := range map[string]string{"desc": strings.Repeat("d", 300), "type": "NUMBER"} {
		entry := md[query].(map[string]any)
		if entry["state"] != "ok" || !reflect.DeepEqual(entry["value"], []any{want}) {
			t.Errorf("within-budget %s lost: %v", query, entry)
		}
	}
}

// Catches: variable churn suppression surviving successful removal and reappearance.
func TestSpecChurnResetOnVariableRemoval(t *testing.T) {
	u := newUPS("a", "device.model", "first")
	f := newFake(t, u)
	cfg := baseCfg()
	cfg.Guard.MaxDistinct = 1
	e := newEnv(t, cfg, named{name: "s", f: f})
	expect(t, e.scrape(), uk("nut_variable_info", "s", "a", "variable", "device.model", "value", "first"), 1)
	f.update(func() { u.vars = []string{varLine("a", "device.model", "second")} })
	got := e.scrape()
	absent(t, got, uk("nut_variable_info", "s", "a", "variable", "device.model", "value", "second"))
	expect(t, got, uk("nut_suppressed", "s", "a", "family", "nut_variable_info", "reason", "churn"), 1)
	f.update(func() { u.vars = nil })
	got = e.scrape()
	expect(t, got, uk("nut_query_success", "s", "a", "query", "vars"), 1)
	absentFamily(t, got, "variable", "device.model", "nut_variable_info")
	f.update(func() { u.vars = []string{varLine("a", "device.model", "third")} })
	got = e.scrape()
	expect(t, got, uk("nut_variable_info", "s", "a", "variable", "device.model", "value", "third"), 1)
	// Zero suppression counters may be omitted from exposition.
	if v := got[uk("nut_suppressed", "s", "a", "family", "nut_variable_info", "reason", "churn")]; v != 0 {
		t.Errorf("churn after removal = %v", v)
	}
}

// Catches: failed live lists hiding cached metadata, null values or command descriptions, and bypassing filters.
func TestSpecCachedMetadataSurvivesListFailures(t *testing.T) {
	u := newUPS("a", "visible", "text", "hidden", "text")
	u.typ = map[string]string{"visible": "ENUM", "hidden": "ENUM"}
	u.enum = map[string][]string{"visible": {"low", "high"}, "hidden": {"secret"}}
	u.rng = map[string][][2]string{"visible": {{"1", "9"}}, "hidden": {{"2", "8"}}}
	u.cmds = []string{"beeper.enable"}
	u.cmdDescs = map[string]string{"beeper.enable": "Enable beeper"}
	f := newFake(t, u)
	cfg := baseCfg()
	cfg.Variables.Exclude = []string{"hidden"}
	e := newEnv(t, cfg, named{name: "s", f: f})
	e.scrape()
	e.startMeta()
	before := e.waitFor(10*time.Second, "cached metadata", func(s samples) bool { return pendingZero(s, "a") })
	families := []string{"nut_variable_type_info", "nut_variable_enum_info", "nut_variable_range_info"}
	expect(t, before, uk(families[0], "s", "a", "variable", "visible", "type", "ENUM"), 1)
	expect(t, before, uk(families[1], "s", "a", "variable", "visible", "value", "low"), 1)
	expect(t, before, uk(families[2], "s", "a", "variable", "visible", "min", "1", "max", "9"), 1)
	cached := invUPS(t, e.inventory(), "s", "a")
	f.update(func() {
		f.override["LIST VAR a"] = fakeErr("ERR DATA-STALE")
		f.override["LIST CMD a"] = fakeErr("ERR DATA-STALE")
	})
	got := e.scrape()
	expect(t, got, uk("nut_query_error_info", "s", "a", "query", "vars", "error", "DATA-STALE"), 1)
	expect(t, got, uk("nut_query_error_info", "s", "a", "query", "commands", "error", "DATA-STALE"), 1)
	compare(t, "cached families during stale lists", onlyFamilies(got, families...), onlyFamilies(before, families...))
	absentFamily(t, got, "variable", "hidden", families...)
	iu := invUPS(t, e.inventory(), "s", "a")
	for _, name := range []string{"visible", "hidden"} {
		v := invVar(t, iu, name)
		if value, ok := v["value"]; !ok || value != nil {
			t.Errorf("%s value = %#v (present %v), want JSON null", name, value, ok)
		}
		md := v["metadata"].(map[string]any)
		if want := invVar(t, cached, name)["metadata"]; !reflect.DeepEqual(md, want) {
			t.Errorf("%s cached metadata changed after failed LIST VAR: got %v, want %v", name, md, want)
		}
		for _, query := range []string{"type", "enum", "range", "desc"} {
			if md[query].(map[string]any)["state"] != "ok" {
				t.Errorf("%s/%s metadata = %v", name, query, md[query])
			}
		}
	}
	cmds, ok := iu["commands"].([]any)
	if !ok || len(cmds) != 1 {
		t.Fatalf("cached commands = %v", iu["commands"])
	}
	cmd := cmds[0].(map[string]any)
	if cmd["name"] != "beeper.enable" || !reflect.DeepEqual(cmd["description"].(map[string]any)["value"], []any{"Enable beeper"}) {
		t.Errorf("cached command = %v", cmd)
	}
}

// Catches: duplicate discoveries consuming max_ups slots or keeping the first description.
func TestSpecDuplicateUPSBeforeCapAndFilter(t *testing.T) {
	for _, excludeA := range []bool{false, true} {
		t.Run(map[bool]string{false: "cap", true: "filter"}[excludeA], func(t *testing.T) {
			old, newer, b := newUPS("a", "x", "1"), newUPS("a", "x", "2"), newUPS("b", "x", "3")
			old.desc = "old"
			newer.desc = "new"
			f := newFake(t, old, newer, b)
			cfg := baseCfg()
			cfg.Guard.MaxUPS = 2
			if excludeA {
				cfg.UPS.Exclude = []string{"a"}
			}
			e := newEnv(t, cfg, named{name: "s", f: f})
			got := e.scrape()
			expect(t, got, k("nut_server_suppressed", "server", "s", "reason", "duplicate"), 1)
			expect(t, got, k("nut_server_suppressed", "server", "s", "reason", "ups_cap"), 0)
			expect(t, got, uk("nut_variable_value", "s", "b", "variable", "x"), 3)
			if excludeA {
				expect(t, got, k("nut_server_suppressed", "server", "s", "reason", "filter"), 1)
			} else {
				expect(t, got, uk("nut_ups_info", "s", "a", "description", "new"), 1)
				expect(t, got, uk("nut_variable_value", "s", "a", "variable", "x"), 2)
			}
			inv := e.inventory()
			servers := inv["servers"].([]any)
			entries := servers[0].(map[string]any)["ups"].([]any)
			dropped := 0
			for _, raw := range entries {
				u := raw.(map[string]any)
				if u["name"] == "a" && u["description"] == "old" {
					dropped++
					if u["collected"] != false || u["skip_reason"] != "duplicate" {
						t.Errorf("dropped entry = %v", u)
					}
				}
			}
			if dropped != 1 {
				t.Errorf("dropped old entries = %d, want 1", dropped)
			}
		})
	}
}

// Catches: serial hanging version queries exhausting the scrape before UPS collection.
func TestSpecHangingVersionsDoNotBlockUPS(t *testing.T) {
	f := newFake(t, newUPS("a", "battery.charge", "83"))
	f.override["VER"] = fakeupsd.Response{Hang: true}
	f.override["NETVER"] = fakeupsd.Response{Hang: true}
	cfg := baseCfg()
	cfg.Timeouts.Command = time.Second
	cfg.Timeouts.Scrape = 1500 * time.Millisecond
	e := newEnv(t, cfg, named{name: "s", f: f})
	got := e.scrape()
	expect(t, got, uk("nut_variable_value", "s", "a", "variable", "battery.charge"), 83)
	for _, q := range []string{"version", "protocol"} {
		expect(t, got, k("nut_server_query_success", "server", "s", "query", q), 0)
	}
}

// Catches: duration captured before collection instead of after discovery and queries.
func TestSpecCollectionDuration(t *testing.T) {
	f := newFake(t, newUPS("a", "x", "1"))
	f.override["LIST UPS"] = fakeupsd.Response{Delay: 150 * time.Millisecond, Lines: []string{"BEGIN LIST UPS", "UPS a \"A\"", "END LIST UPS"}}
	cfg := baseCfg()
	cfg.Timeouts.Command = time.Second
	got := newEnv(t, cfg, named{name: "s", f: f}).scrape()
	expect(t, got, uk("nut_variable_value", "s", "a", "variable", "x"), 1)
	if v, ok := got[k("nut_collect_duration_seconds", "server", "s")]; !ok || !(v >= 0.15) {
		t.Errorf("duration = %v (present %v), want >= 0.15", v, ok)
	}
}

// Catches: mid-list DATA-STALE being projected as generic protocol failure.
func TestSpecMidListErrorMetric(t *testing.T) {
	u := newUPS("a")
	u.rawVars = []string{"BEGIN LIST VAR a", varLine("a", "x", "1"), "ERR DATA-STALE"}
	got := newEnv(t, baseCfg(), named{name: "s", f: newFake(t, u)}).scrape()
	expect(t, got, uk("nut_query_error_info", "s", "a", "query", "vars", "error", "DATA-STALE"), 1)
	absent(t, got, uk("nut_variable_value", "s", "a", "variable", "x"))
}

// Catches: duplicate RW names silently disappearing without an exact suppression count.
func TestSpecWritableDuplicateCount(t *testing.T) {
	u := newUPS("a", "x", "1")
	u.rw = []string{rwLine("a", "x", "1"), rwLine("a", "x", "1")}
	got := newEnv(t, baseCfg(), named{name: "s", f: newFake(t, u)}).scrape()
	expect(t, got, uk("nut_suppressed", "s", "a", "family", "nut_variable_writable", "reason", "duplicate"), 1)
	compare(t, "one writable series", onlyFamilies(got, "nut_variable_writable"), samples{uk("nut_variable_writable", "s", "a", "variable", "x"): 1})
}

// Catches: filtered enum suppression counting entries instead of distinct emitted series.
func TestSpecFilteredEnumCount(t *testing.T) {
	u := newUPS("a", "x", "1")
	u.enum = map[string][]string{"x": {"a", "b", "c"}}
	cfg := baseCfg()
	cfg.Variables.Exclude = []string{"x"}
	e := newEnv(t, cfg, named{name: "s", f: newFake(t, u)})
	e.scrape()
	e.startMeta()
	got := e.waitFor(10*time.Second, "filtered enum cached", func(s samples) bool { return pendingZero(s, "a") })
	expect(t, got, uk("nut_suppressed", "s", "a", "family", "nut_variable_enum_info", "reason", "filter"), 3)
	absentFamily(t, got, "variable", "x", "nut_variable_enum_info")
	md := invVar(t, invUPS(t, e.inventory(), "s", "a"), "x")["metadata"].(map[string]any)
	if !reflect.DeepEqual(md["enum"].(map[string]any)["value"], []any{"a", "b", "c"}) {
		t.Errorf("enum not cached: %v", md["enum"])
	}
}

// Catches: missing server_info when both version queries fail or one label exceeds max_length.
func TestSpecServerInfoAlwaysEmitted(t *testing.T) {
	for _, long := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed", true: "long"}[long], func(t *testing.T) {
			f := newFake(t, newUPS("a", "x", "1"))
			cfg := baseCfg()
			cfg.Guard.MaxLength = 16
			protocol := ""
			if long {
				f.ver = strings.Repeat("v", 17)
				f.netver = "1.2"
				protocol = "1.2"
			} else {
				f.override["VER"] = fakeErr("ERR UNKNOWN-COMMAND")
				f.override["NETVER"] = fakeErr("ERR UNKNOWN-COMMAND")
			}
			got := newEnv(t, cfg, named{name: "s", f: f}).scrape()
			compare(t, "one server info", onlyFamilies(got, "nut_server_info"), samples{k("nut_server_info", "server", "s", "version", "", "protocol", protocol): 1})
			expect(t, got, uk("nut_variable_value", "s", "a", "variable", "x"), 1)
		})
	}
}

// Catches: type, enum and range churn keys surviving deletion of their variable.
func TestSpecMetadataChurnResetOnVariableRemoval(t *testing.T) {
	u := newUPS("a", "x", "1")
	u.typ = map[string]string{"x": "STRING:1"}
	u.enum = map[string][]string{"x": {"first"}}
	u.rng = map[string][][2]string{"x": {{"0", "1"}}}
	f := newFake(t, u)
	cfg := baseCfg()
	cfg.Guard.MaxDistinct = 1
	cfg.MetadataTTL = time.Second
	e := newEnv(t, cfg, named{name: "s", f: f})
	e.scrape()
	e.startMeta()
	got := e.waitFor(10*time.Second, "initial metadata", func(s samples) bool { return pendingZero(s, "a") })
	families := []string{"nut_variable_type_info", "nut_variable_enum_info", "nut_variable_range_info"}
	expect(t, got, uk(families[0], "s", "a", "variable", "x", "type", "STRING:1"), 1)
	expect(t, got, uk(families[1], "s", "a", "variable", "x", "value", "first"), 1)
	expect(t, got, uk(families[2], "s", "a", "variable", "x", "min", "0", "max", "1"), 1)
	f.update(func() {
		u.typ["x"] = "STRING:2"
		u.enum["x"] = []string{"second"}
		u.rng["x"] = [][2]string{{"2", "3"}}
	})
	e.waitFor(10*time.Second, "all metadata families churn suppressed", func(s samples) bool {
		for _, family := range families {
			if s[uk("nut_suppressed", "s", "a", "family", family, "reason", "churn")] != 1 {
				return false
			}
		}
		return true
	})
	f.update(func() { u.vars = nil })
	got = e.scrape()
	expect(t, got, uk("nut_query_success", "s", "a", "query", "vars"), 1)
	absentFamily(t, got, "variable", "x", families...)
	f.update(func() {
		u.vars = []string{varLine("a", "x", "1")}
		u.typ["x"] = "STRING:3"
		u.enum["x"] = []string{"third"}
		u.rng["x"] = [][2]string{{"4", "5"}}
	})
	e.scrape()
	// Wait for cache state, not projection: a broken churn reset must reach the assertions.
	e.waitFor(10*time.Second, "replacement metadata refreshed", func(s samples) bool { return pendingZero(s, "a") })
	got = e.scrape()
	expect(t, got, uk(families[0], "s", "a", "variable", "x", "type", "STRING:3"), 1)
	expect(t, got, uk(families[1], "s", "a", "variable", "x", "value", "third"), 1)
	expect(t, got, uk(families[2], "s", "a", "variable", "x", "min", "4", "max", "5"), 1)
	for _, family := range families {
		if v := got[uk("nut_suppressed", "s", "a", "family", family, "reason", "churn")]; v != 0 {
			t.Errorf("%s churn after removal = %v", family, v)
		}
	}
}
