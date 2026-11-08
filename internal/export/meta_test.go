package export_test

import (
	"slices"
	"testing"
	"time"
)

func metaUPS(name string) *upsSpec {
	u := newUPS(name, "input.sensitivity", "medium", "ups.delay.shutdown", "20", "device.model", "X1")
	u.cmds = []string{"beeper.enable"}
	u.typ = map[string]string{"input.sensitivity": "RW ENUM", "device.model": "STRING:20", "ups.delay.shutdown": "RANGE"}
	u.enum = map[string][]string{"input.sensitivity": {"low", "medium", "high"}}
	// A variable with both an enum and a range list keeps both (2.7.4 GET TYPE hides the range).
	u.rng = map[string][][2]string{"input.sensitivity": {{"100", "200"}}, "ups.delay.shutdown": {{"0", "600"}}}
	u.descs = map[string]string{"input.sensitivity": "Input power sensitivity"}
	u.cmdDescs = map[string]string{"beeper.enable": "Enable the UPS beeper"}
	return u
}

func pendingZero(s samples, ups string) bool {
	n := 0
	for key, v := range onlyFamilies(withLabel(s, "ups", ups), "nut_metadata_pending") {
		n++
		if v != 0 {
			return false
		}
		_ = key
	}
	return n > 0
}

func strs(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// Catches: metadata missing from metrics/inventory, GET TYPE hiding the range list of an enum variable,
// metadata queried on the scrape path, and `complete` ignoring metadata.
func TestMetadataExported(t *testing.T) {
	u := metaUPS("ups")
	f := newFake(t, u)
	e := newEnv(t, baseCfg(), named{name: "nas", f: f})
	e.scrape()
	e.startMeta()
	got := e.waitFor(15*time.Second, "metadata refreshed", func(s samples) bool { return pendingZero(s, "ups") })

	ty := func(v, ty string) string {
		return uk("nut_variable_type_info", "nas", "ups", "variable", v, "type", ty)
	}
	compare(t, "type_info", onlyFamilies(got, "nut_variable_type_info"), samples{
		ty("input.sensitivity", "RW"): 1, ty("input.sensitivity", "ENUM"): 1,
		ty("device.model", "STRING:20"): 1, ty("ups.delay.shutdown", "RANGE"): 1,
	})
	en := func(v string) string {
		return uk("nut_variable_enum_info", "nas", "ups", "variable", "input.sensitivity", "value", v)
	}
	compare(t, "enum_info", onlyFamilies(got, "nut_variable_enum_info"), samples{en("low"): 1, en("medium"): 1, en("high"): 1})
	rg := func(v, lo, hi string) string {
		return uk("nut_variable_range_info", "nas", "ups", "variable", v, "min", lo, "max", hi)
	}
	compare(t, "range_info", onlyFamilies(got, "nut_variable_range_info"), samples{
		rg("input.sensitivity", "100", "200"): 1, rg("ups.delay.shutdown", "0", "600"): 1,
	})
	expect(t, got, uk("nut_metadata_errors", "nas", "ups", "query", "enum"), 0)

	inv := e.inventory()
	iu := invUPS(t, inv, "nas", "ups")
	md := invVar(t, iu, "input.sensitivity")["metadata"].(map[string]any)
	val := func(q string) []string { return strs(md[q].(map[string]any)["value"]) }
	if !slices.Equal(val("enum"), []string{"low", "medium", "high"}) {
		t.Errorf("inventory enum = %v", val("enum"))
	}
	if r := val("range"); !slices.Contains(r, "100") || !slices.Contains(r, "200") {
		t.Errorf("inventory range = %v", r)
	}
	if !slices.Equal(val("type"), []string{"RW", "ENUM"}) {
		t.Errorf("inventory type = %v", val("type"))
	}
	if !slices.Equal(val("desc"), []string{"Input power sensitivity"}) {
		t.Errorf("inventory desc = %v", val("desc"))
	}
	cmds := iu["commands"].([]any)
	if len(cmds) != 1 || !slices.Equal(strs(cmds[0].(map[string]any)["description"].(map[string]any)["value"]), []string{"Enable the UPS beeper"}) {
		t.Errorf("inventory commands = %v", cmds)
	}
	if iu["complete"] != true {
		t.Errorf("complete = %v with every core query and metadata entry ok", iu["complete"])
	}
	// Descriptions are inventory-only (design section 5).
	absentFamily(t, got, "ups", "ups", "nut_variable_desc_info")
}

// Catches: one UPS's hanging metadata blocking another's, metadata blocking the scrape, and the core
// data of the UPS with failing metadata disappearing.
func TestMetadataHangIsolated(t *testing.T) {
	bad, good := metaUPS("bad"), metaUPS("good")
	bad.hangMeta = true
	f := newFake(t, bad, good)
	e := newEnv(t, baseCfg(), named{name: "nas", f: f})
	e.scrape()
	e.startMeta()
	got := e.waitFor(15*time.Second, "good UPS metadata", func(s samples) bool { return pendingZero(s, "good") })
	expect(t, got, uk("nut_variable_enum_info", "nas", "good", "variable", "input.sensitivity", "value", "high"), 1)
	start := time.Now()
	got = e.scrape()
	if d := time.Since(start); d > time.Second {
		t.Errorf("scrape took %v while metadata hangs", d)
	}
	expect(t, got, uk("nut_ups_up", "nas", "bad"), 1)
	expect(t, got, uk("nut_variable_info", "nas", "bad", "variable", "device.model", "value", "X1"), 1)
	absentFamily(t, got, "ups", "bad", "nut_variable_enum_info", "nut_variable_type_info", "nut_variable_range_info")
}

// Catches: a changed enum not replacing the old one, ENUM->none not clearing, and a failed refresh
// wiping the previous value instead of keeping it and counting the error (design section 3).
func TestMetadataRefreshSemantics(t *testing.T) {
	u := metaUPS("ups")
	f := newFake(t, u)
	cfg := baseCfg()
	cfg.MetadataTTL = 200 * time.Millisecond
	e := newEnv(t, cfg, named{name: "nas", f: f})
	e.scrape()
	e.startMeta()
	en := func(v string) string {
		return uk("nut_variable_enum_info", "nas", "ups", "variable", "input.sensitivity", "value", v)
	}
	const wait = 25 * time.Second
	e.waitFor(wait, "initial enum", func(s samples) bool { _, ok := s[en("high")]; return ok })

	f.update(func() { u.enum["input.sensitivity"] = []string{"low", "medium"} })
	got := e.waitFor(wait, "enum change", func(s samples) bool { _, ok := s[en("high")]; return !ok && s[en("medium")] == 1 })
	expect(t, got, en("low"), 1)

	f.update(func() { u.enum["input.sensitivity"] = nil })
	e.waitFor(wait, "enum cleared", func(s samples) bool { return len(onlyFamilies(s, "nut_variable_enum_info")) == 0 })

	f.update(func() { u.enum["input.sensitivity"] = []string{"a", "b"} })
	e.waitFor(wait, "enum a,b", func(s samples) bool { return s[en("a")] == 1 })
	f.update(func() { f.override["LIST ENUM ups input.sensitivity"] = fakeErr("ERR VAR-NOT-SUPPORTED") })
	got = e.waitFor(wait, "enum error counted", func(s samples) bool {
		return s[uk("nut_metadata_errors", "nas", "ups", "query", "enum")] >= 1
	})
	expect(t, got, en("a"), 1)
	expect(t, got, en("b"), 1)
}
