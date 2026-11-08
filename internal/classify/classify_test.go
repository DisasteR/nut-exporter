package classify

import (
	"os"
	"strings"
	"testing"
)

// Catches numeric guessing on opaque fields and missing fixture variables.
func TestFixture(t *testing.T) {
	expected := map[string]struct {
		kind  Kind
		value float64
	}{
		"battery.charge": {Gauge, 100}, "battery.charge.low": {Gauge, 10}, "battery.charge.warning": {Gauge, 50},
		"battery.date": {Text, 0}, "battery.mfr.date": {Text, 0}, "battery.runtime": {Gauge, 25500},
		"battery.runtime.low": {Gauge, 120}, "battery.type": {Text, 0}, "battery.voltage": {Gauge, 27.3},
		"battery.voltage.nominal": {Gauge, 24}, "device.mfr": {Text, 0}, "device.model": {Text, 0},
		"device.serial": {Text, 0}, "device.type": {Text, 0}, "driver.name": {Text, 0},
		"driver.parameter.pollfreq": {Text, 0}, "driver.parameter.pollinterval": {Text, 0},
		"driver.parameter.port": {Text, 0}, "driver.parameter.synchronous": {Text, 0},
		"driver.version": {Text, 0}, "driver.version.data": {Text, 0}, "driver.version.internal": {Text, 0},
		"input.sensitivity": {Text, 0}, "input.transfer.high": {Gauge, 294}, "input.transfer.low": {Gauge, 176},
		"input.voltage": {Gauge, 237}, "input.voltage.nominal": {Gauge, 230}, "ups.beeper.status": {Text, 0},
		"ups.delay.shutdown": {Gauge, 20}, "ups.firmware": {Text, 0}, "ups.firmware.aux": {Text, 0},
		"ups.load": {Gauge, 0}, "ups.mfr": {Text, 0}, "ups.mfr.date": {Text, 0}, "ups.model": {Text, 0},
		"ups.productid": {Text, 0}, "ups.realpower.nominal": {Gauge, 865}, "ups.serial": {Text, 0},
		"ups.status": {Text, 0}, "ups.test.result": {Text, 0}, "ups.timer.reboot": {Gauge, 0},
		"ups.timer.shutdown": {Gauge, -1}, "ups.vendorid": {Text, 0},
	}
	data, err := os.ReadFile("../../testdata/apc-back-ups-rs-1500g.txt")
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "VAR ups ") {
			continue
		}
		name, quoted, ok := strings.Cut(strings.TrimPrefix(line, "VAR ups "), " ")
		if !ok || len(quoted) < 2 || quoted[0] != '"' || quoted[len(quoted)-1] != '"' {
			t.Fatalf("bad fixture line: %q", line)
		}
		want, ok := expected[name]
		if !ok {
			t.Fatalf("unexpected variable %q", name)
		}
		kind, v, parseErr := c.Classify(name, quoted[1:len(quoted)-1])
		if kind != want.kind || v != want.value || parseErr {
			t.Errorf("%s: got (%v,%v,%v), want %v", name, kind, v, parseErr, want)
		}
		delete(expected, name)
		count++
	}
	if count != 43 || len(expected) != 0 {
		t.Fatalf("count=%d missing=%v", count, expected)
	}
}

// Catches ParseFloat-only classification and numeric allow lists.
func TestGrammar(t *testing.T) {
	c, _ := New(nil, nil)
	for _, value := range []string{"1e3", "+1", "1.", ".5", " 1", "1 ", "1,5", "0x10", "0x1p3", "Inf", "NaN", "", "hello", "--1", "1\n", "１"} {
		k, v, e := c.Classify("experimental.foo.bar", value)
		if k != Text || v != 0 || e {
			t.Errorf("%q: (%v,%v,%v)", value, k, v, e)
		}
	}
	for value, want := range map[string]float64{"01200.2": 1200.2, "-12.5": -12.5, "0": 0, "12.5": 12.5} {
		k, v, e := c.Classify("experimental.foo.bar", value)
		if k != Gauge || v != want || e {
			t.Errorf("%q: (%v,%v,%v)", value, k, v, e)
		}
	}
}

// Catches reversed precedence, unvalidated patterns and aliased configuration.
func TestOverrides(t *testing.T) {
	numeric := []string{"ups.*", "forced.?"}
	texts := []string{"*.load"}
	c, err := New(numeric, texts)
	if err != nil {
		t.Fatal(err)
	}
	numeric[0] = "nothing"
	texts[0] = "nothing"
	for _, tt := range []struct {
		name, value string
		kind        Kind
		v           float64
		parseErr    bool
	}{
		{"ups.load", "12", Text, 0, false}, {"ups.load", "bad", Text, 0, false},
		{"ups.productid", "0002", Gauge, 2, false}, {"ups.foo", "bad", Text, 0, true},
		{"forced.x", "bad", Text, 0, true}, {"forced.xx", "bad", Text, 0, false},
	} {
		k, v, e := c.Classify(tt.name, tt.value)
		if k != tt.kind || v != tt.v || e != tt.parseErr {
			t.Errorf("%+v: (%v,%v,%v)", tt, k, v, e)
		}
	}
	for _, p := range []string{"[", "a[", "\\"} {
		if _, err := New([]string{p}, nil); err == nil {
			t.Errorf("numeric %q accepted", p)
		}
		if _, err := New(nil, []string{p}); err == nil {
			t.Errorf("text %q accepted", p)
		}
	}
	c, _ = New(nil, []string{"experimental.*"})
	if k, _, e := c.Classify("experimental.foo", "12"); k != Text || e {
		t.Fatal("text override ignored")
	}
}

// Catches omitted opaque patterns, even when real fixture values are already text.
func TestOpaque(t *testing.T) {
	names := []string{"x.serial", "x.id", "x.vendorid", "x.productid", "x.date", "x.date.foo", "x.time", "x.firmware", "x.firmware.foo", "x.version", "x.version.foo", "x.mfr", "x.model", "x.name", "x.desc", "x.description", "x.contact", "x.location", "x.part", "x.macaddr", "x.type", "x.status", "x.alarm", "x.result", "x.reason", "x.sensitivity", "x.mode", "x.feed.color", "driver.parameter.foo", "driver.flag.foo", "driver.state"}
	c, _ := New(nil, nil)
	for _, name := range names {
		if k, _, e := c.Classify(name, "123"); k != Text || e {
			t.Errorf("%s not opaque", name)
		}
	}
}

// Catches date normalization, mixed separators and missing name checks.
func TestTimestamp(t *testing.T) {
	for _, tt := range []struct {
		name, value string
		want        int64
		ok          bool
	}{
		{"battery.date", "2026/11/06", 1793923200, true},
		{"ups.mfr.date", "2022-06-03", 1654214400, true},
		{"x.date.foo", "2024-02-29", 1709164800, true},
		{"x.date", "2026-99-99", 0, false}, {"x.date", "2026/11-08", 0, false},
		{"x.date", "11/07/26", 0, false}, {"x.date", "2025-02-29", 0, false},
		{"x.date", "2026-1-06", 0, false}, {"x.date", "2026-11-06 ", 0, false},
		{"x.voltage", "2026/11/06", 0, false}, {"x.dateish", "2026/11/06", 0, false},
	} {
		v, ok := Timestamp(tt.name, tt.value)
		if v != tt.want || ok != tt.ok {
			t.Errorf("%+v: (%d,%v)", tt, v, ok)
		}
	}
}
