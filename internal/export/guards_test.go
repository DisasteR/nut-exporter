// Copyright 2026 The nut-exporter Authors
// SPDX-License-Identifier: Apache-2.0

package export_test

import (
	"strings"
	"testing"

	"github.com/DisasteR/nut-exporter/internal/config"
	"github.com/DisasteR/nut-exporter/internal/fakeupsd"
)

// Catches: a long value truncated or hashed instead of dropped, a guard that is silent, and a guard that
// also hides short values or gauges.
func TestLengthGuard(t *testing.T) {
	long := strings.Repeat("x", 30)
	f := newFake(t, newUPS("ups", "device.contact", long, "device.model", "short", "battery.charge", "100"))
	cfg := baseCfg()
	cfg.Guard.MaxLength = 20
	e := newEnv(t, cfg, named{name: "nas", f: f})
	got := e.scrape()
	absentFamily(t, got, "variable", "device.contact", "nut_variable_info")
	for key := range got {
		if strings.Contains(key, "xxxxxxxx") {
			t.Errorf("part of the long value exported: %s", key)
		}
	}
	expect(t, got, uk("nut_suppressed", "nas", "ups", "family", "nut_variable_info", "reason", "length"), 1)
	expect(t, got, uk("nut_variable_info", "nas", "ups", "variable", "device.model", "value", "short"), 1)
	expect(t, got, uk("nut_battery_charge_percent", "nas", "ups"), 100)
	// The value stays whole in the inventory.
	if v := invVar(t, invUPS(t, e.inventory(), "nas", "ups"), "device.contact")["value"]; v != long {
		t.Errorf("inventory value = %v", v)
	}
}

// Catches: no churn guard (unbounded series), churn counted across variables, or a guard that does not
// report itself.
func TestChurnGuard(t *testing.T) {
	u := newUPS("ups", "ups.test.result", "r0", "device.model", "stable")
	f := newFake(t, u)
	cfg := baseCfg()
	cfg.Guard.MaxDistinct = 3
	cfg.Guard.ChurnWindow = 3600e9
	e := newEnv(t, cfg, named{name: "nas", f: f})
	sup := uk("nut_suppressed", "nas", "ups", "family", "nut_variable_info", "reason", "churn")
	for i, v := range []string{"r1", "r2", "r3", "r4", "r5"} {
		f.update(func() { u.vars[0] = varLine("ups", "ups.test.result", v) })
		got := e.scrape()
		expect(t, got, uk("nut_variable_info", "nas", "ups", "variable", "device.model", "value", "stable"), 1)
		if i < 3 { // 3 distinct values <= max_distinct
			expect(t, got, uk("nut_variable_info", "nas", "ups", "variable", "ups.test.result", "value", v), 1)
			absent(t, got, sup)
		} else { // more than 3 distinct values within the window
			absentFamily(t, got, "variable", "ups.test.result", "nut_variable_info")
			expect(t, got, sup, 1)
		}
	}
}

// Catches: max_series ignored, dropping in name order instead of reverse, dropping gauges only, dropping
// variables before commands/clients, and silent drops.
func TestSeriesCap(t *testing.T) {
	cfg := baseCfg()
	cfg.Guard.MaxSeries = 2
	t.Run("variables reverse name order", func(t *testing.T) {
		u := newUPS("ups", "battery.charge", "100", "device.model", "m", "input.voltage", "230", "ups.load", "5", "ups.mfr", "apc")
		u.override = map[string]fakeupsd.Response{"GET NUMLOGINS ups": {Lines: []string{"ERR DATA-STALE"}}}
		e := newEnv(t, cfg, named{name: "nas", f: newFake(t, u)})
		got := e.scrape()
		budget := onlyFamilies(got, append(gaugeFamilies(), "nut_variable_info")...)
		compare(t, "kept", budget, samples{
			uk("nut_battery_charge_percent", "nas", "ups"):                                  100,
			uk("nut_variable_info", "nas", "ups", "variable", "device.model", "value", "m"): 1,
		})
		expect(t, got, uk("nut_suppressed", "nas", "ups", "family", "nut_input_voltage_volts", "reason", "series_cap"), 1)
		expect(t, got, uk("nut_suppressed", "nas", "ups", "family", "nut_ups_load_percent", "reason", "series_cap"), 1)
		expect(t, got, uk("nut_suppressed", "nas", "ups", "family", "nut_variable_info", "reason", "series_cap"), 1)
		expect(t, got, uk("nut_ups_up", "nas", "ups"), 1) // health outside the budget
	})
	t.Run("commands and clients go before variables", func(t *testing.T) {
		u := newUPS("ups", "battery.charge", "100", "input.voltage", "230")
		u.cmds = []string{"beeper.enable"}
		u.clients = []string{"10.0.0.1"}
		u.override = map[string]fakeupsd.Response{"GET NUMLOGINS ups": {Lines: []string{"ERR DATA-STALE"}}}
		e := newEnv(t, cfg, named{name: "nas", f: newFake(t, u)})
		got := e.scrape()
		compare(t, "kept", onlyFamilies(got, append(gaugeFamilies(), "nut_ups_command_info", "nut_ups_client_sessions")...), samples{
			uk("nut_battery_charge_percent", "nas", "ups"):           100,
			uk("nut_input_voltage_volts", "nas", "ups", "phase", ""): 230,
		})
		expect(t, got, uk("nut_suppressed", "nas", "ups", "family", "nut_ups_command_info", "reason", "series_cap"), 1)
		expect(t, got, uk("nut_suppressed", "nas", "ups", "family", "nut_ups_client_sessions", "reason", "series_cap"), 1)
	})
}

var _ = config.Default
