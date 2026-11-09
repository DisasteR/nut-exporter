// Copyright 2026 Benjamin Saiz
// SPDX-License-Identifier: Apache-2.0

package export_test

import (
	"testing"
	"time"
)

// devKey is the nut_device_info key of a UPS on server "nas"; unset labels are empty.
func devKey(ups string, set map[string]string) string { return devKeyOn("nas", ups, set) }

func devKeyOn(server, ups string, set map[string]string) string {
	var kv []string
	for _, l := range []string{"model", "mfr", "serial", "type", "part", "macaddr"} {
		kv = append(kv, l, set[l])
	}
	return uk("nut_device_info", server, ups, kv...)
}

// Catches: ups.* preferred over device.*, values trimmed or altered, no fallback for drivers without
// device.* or with an empty one, a fallback beyond the three entries NUT duplicates, administrative
// text (description, contact, location) used as labels, the first of repeated values winning, a
// series for a UPS without identity or with only empty values, and one surviving a failed LIST VAR.
func TestDeviceInfo(t *testing.T) {
	f := newFake(t,
		newUPS("both", "device.model", "Back-UPS RS 1500G", "ups.model", "Old name", "device.mfr", "APC",
			"ups.mfr", "American Power Conversion", "device.serial", "XX0000X00000  ", "ups.serial", "other",
			"device.type", "ups", "device.part", "0002", "device.macaddr", "68:b5:99:f5:89:27",
			"device.description", "rack 1", "device.contact", "John", "device.location", "1st floor"),
		newUPS("legacy", "ups.model", "SMART-UPS 700", "ups.mfr", "APC", "ups.serial", " 0042 ",
			"ups.id", "rack1", "ups.firmware", "6.2"),
		newUPS("emptydev", "device.model", "", "ups.model", "Legacy model", "device.serial", "S1"),
		newUPS("dup", "device.model", "first", "device.model", "second"),
		newUPS("none", "battery.charge", "100", "ups.firmware", "1.0", "device.contact", "John"),
		newUPS("blank", "device.model", "", "ups.serial", ""),
		newUPS("down", "device.model", "x"),
	)
	f.override["LIST VAR down"] = fakeErr("ERR DATA-STALE")
	e := newEnv(t, baseCfg(), named{name: "nas", f: f})
	got := onlyFamilies(e.scrape(), "nut_device_info")

	want := samples{
		devKey("both", map[string]string{"model": "Back-UPS RS 1500G", "mfr": "APC", "serial": "XX0000X00000  ",
			"type": "ups", "part": "0002", "macaddr": "68:b5:99:f5:89:27"}): 1,
		devKey("legacy", map[string]string{"model": "SMART-UPS 700", "mfr": "APC", "serial": " 0042 "}): 1,
		devKey("emptydev", map[string]string{"model": "Legacy model", "serial": "S1"}):                  1,
		devKey("dup", map[string]string{"model": "second"}):                                             1,
	}
	compare(t, "device", got, want)
}

// Catches: a filtered variable leaking into the labels, and a filtered device.* not falling back to
// the ups.* entry still exported.
func TestDeviceInfoFilter(t *testing.T) {
	cfg := baseCfg()
	cfg.Variables.Exclude = []string{"device.serial", "device.mfr", "ups.mfr"}
	f := newFake(t, newUPS("a", "device.serial", "D1", "ups.serial", "U1", "device.mfr", "M", "ups.mfr", "M2",
		"device.model", "X"))
	e := newEnv(t, cfg, named{name: "nas", f: f})
	compare(t, "filter", onlyFamilies(e.scrape(), "nut_device_info"),
		samples{devKey("a", map[string]string{"model": "X", "serial": "U1"}): 1})
}

// Catches: an over-long or non-UTF-8 identity value exported as a label instead of the series being
// counted as suppressed.
func TestDeviceInfoLabelGuard(t *testing.T) {
	cfg := baseCfg()
	cfg.Guard.MaxLength = 8
	f := newFake(t, newUPS("a", "device.model", "a model name longer than eight"), newUPS("b", "device.model", "\xff"))
	e := newEnv(t, cfg, named{name: "nas", f: f})
	got := e.scrape()
	if n := len(onlyFamilies(got, "nut_device_info")); n != 0 {
		t.Errorf("invalid labels exported: %v", onlyFamilies(got, "nut_device_info"))
	}
	expect(t, got, uk("nut_suppressed", "nas", "a", "family", "nut_device_info", "reason", "length"), 1)
	expect(t, got, uk("nut_suppressed", "nas", "b", "family", "nut_device_info", "reason", "encoding"), 1)
}

// Catches: no churn guard on the identity (a rotating serial adding a series per scrape), and a
// series that does not come back once LIST VAR recovers.
func TestDeviceInfoChurnAndRecovery(t *testing.T) {
	u := newUPS("a", "device.model", "M", "device.serial", "s0")
	f := newFake(t, u)
	cfg := baseCfg()
	cfg.Guard.MaxDistinct = 2
	e := newEnv(t, cfg, named{name: "s", f: f})
	churn := uk("nut_suppressed", "s", "a", "family", "nut_device_info", "reason", "churn")
	for _, serial := range []string{"s0", "s1", "s2"} { // two values gone within the window at s2
		f.update(func() { u.vars = []string{varLine("a", "device.model", "M"), varLine("a", "device.serial", serial)} })
		got := e.scrape()
		key := devKeyOn("s", "a", map[string]string{"model": "M", "serial": serial})
		if serial != "s2" {
			expect(t, got, key, 1)
			continue
		}
		absent(t, got, key)
		expect(t, got, churn, 1)
	}

	r := newUPS("b", "device.model", "R")
	g := newFake(t, r)
	e = newEnv(t, baseCfg(), named{name: "s", f: g})
	key := devKeyOn("s", "b", map[string]string{"model": "R"})
	expect(t, e.scrape(), key, 1)
	g.update(func() { g.override["LIST VAR b"] = fakeErr("ERR DATA-STALE") })
	absent(t, e.scrape(), key)
	g.update(func() { delete(g.override, "LIST VAR b") })
	expect(t, e.scrape(), key, 1)
}

// Catches: nut_device_info counted in, and dropped by, the max_series budget it is documented to
// stay outside of.
func TestDeviceInfoOutsideBudget(t *testing.T) {
	cfg := baseCfg()
	cfg.Guard.MaxSeries = 1
	f := newFake(t, newUPS("a", "device.model", "M", "battery.charge", "100", "battery.voltage", "27.3"))
	e := newEnv(t, cfg, named{name: "nas", f: f})
	got := e.scrape()
	expect(t, got, devKey("a", map[string]string{"model": "M"}), 1)
	if n := len(onlyFamilies(got, "nut_variable_info", "nut_battery_charge_percent", "nut_battery_voltage_volts")); n != 1 {
		t.Errorf("variable series under max_series=1: %d, want 1", n)
	}
}

// Catches: device churn history kept after the UPS disappears, or never expiring with the window.
func TestDeviceInfoChurnReset(t *testing.T) {
	ident := func(u *upsSpec, serial string) {
		u.vars = []string{varLine(u.name, "device.model", "M"), varLine(u.name, "device.serial", serial)}
	}
	key := func(serial string) string {
		return devKeyOn("s", "a", map[string]string{"model": "M", "serial": serial})
	}
	churn := uk("nut_suppressed", "s", "a", "family", "nut_device_info", "reason", "churn")

	u := newUPS("a")
	ident(u, "s0")
	f := newFake(t, u)
	cfg := baseCfg()
	cfg.Guard.MaxDistinct = 1
	e := newEnv(t, cfg, named{name: "s", f: f})
	expect(t, e.scrape(), key("s0"), 1)
	f.update(func() { ident(u, "s1") })
	expect(t, e.scrape(), churn, 1)
	f.update(func() { f.ups = nil })
	e.scrape()
	f.update(func() { f.ups = []*upsSpec{u}; ident(u, "s2") })
	got := e.scrape()
	expect(t, got, key("s2"), 1)
	absent(t, got, churn)

	v := newUPS("a")
	ident(v, "s0")
	g := newFake(t, v)
	cfg.Guard.ChurnWindow = 300 * time.Millisecond
	e = newEnv(t, cfg, named{name: "s", f: g})
	expect(t, e.scrape(), key("s0"), 1)
	g.update(func() { ident(v, "s1") })
	expect(t, e.scrape(), churn, 1)
	time.Sleep(400 * time.Millisecond)
	expect(t, e.scrape(), key("s1"), 1)
}
