// Copyright 2026 Benjamin Saiz
// SPDX-License-Identifier: Apache-2.0

package export_test

import (
	"context"
	"strings"
	"testing"
)

// Catches: catalogue variables exported under the generic family (or twice), wrong index/phase
// labels, opaque names turned into gauges, and HELP/TYPE missing for catalogue families.
func TestCatalogueNamedGauges(t *testing.T) {
	u := newUPS("ups", "battery.charge", "100", "input.voltage", "230", "input.L1-N.voltage", "231.5",
		"ambient.2.temperature", "25.5", "outlet.group.1.current", "0.50", "experimental.x", "3",
		"ups.serial", "0002")
	e := newEnv(t, baseCfg(), named{name: "nas", f: newFake(t, u)})
	rec := e.serve(context.Background(), "/metrics", e.exp.ServeMetrics)
	body := rec.Body.String()
	got := parse(t, strings.NewReader(body))
	compare(t, "gauges", onlyFamilies(got, gaugeFamilies()...), samples{
		uk("nut_battery_charge_percent", "nas", "ups"):                            100,
		uk("nut_input_voltage_volts", "nas", "ups", "phase", ""):                  230,
		uk("nut_input_voltage_volts", "nas", "ups", "phase", "L1-N"):              231.5,
		uk("nut_ambient_temperature_celsius", "nas", "ups", "ambient", "2"):       25.5,
		uk("nut_outlet_group_current_amperes", "nas", "ups", "outlet_group", "1"): 0.5,
		uk("nut_variable_value", "nas", "ups", "variable", "experimental.x"):      3,
	})
	expect(t, got, uk("nut_variable_info", "nas", "ups", "variable", "ups.serial", "value", "0002"), 1)
	for _, want := range []string{
		"# HELP nut_battery_charge_percent NUT battery.charge (catalogue unit: percent).",
		"# TYPE nut_battery_charge_percent gauge",
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing %q", want)
		}
	}
	inv := invUPS(t, e.inventory(), "nas", "ups")
	for name, want := range map[string]string{
		"battery.charge": "nut_battery_charge_percent", "input.L1-N.voltage": "nut_input_voltage_volts",
		"experimental.x": "nut_variable_value", "ups.serial": "nut_variable_info",
	} {
		if got := invVar(t, inv, name)["metric"]; got != want {
			t.Errorf("inventory %s metric = %v, want %s", name, got, want)
		}
	}
}

// Catches: a catalogue variable whose value is not numeric exported as a gauge or lost, and
// filters not applied to catalogue families.
func TestCatalogueTextAndFilter(t *testing.T) {
	u := newUPS("ups", "battery.charge", "n/a", "input.voltage", "230", "ups.load", "12")
	cfg := baseCfg()
	cfg.Variables.Exclude = []string{"input.*"}
	e := newEnv(t, cfg, named{name: "nas", f: newFake(t, u)})
	got := e.scrape()
	expect(t, got, uk("nut_variable_info", "nas", "ups", "variable", "battery.charge", "value", "n/a"), 1)
	absent(t, got, uk("nut_battery_charge_percent", "nas", "ups"))
	absent(t, got, uk("nut_input_voltage_volts", "nas", "ups", "phase", ""))
	expect(t, got, uk("nut_suppressed", "nas", "ups", "family", "nut_input_voltage_volts", "reason", "filter"), 1)
	expect(t, got, uk("nut_ups_load_percent", "nas", "ups"), 12)
}

// Catches: classification overrides ignored for catalogue names, and the default sensor forms
// ("" and "0") or another index colliding in one scrape.
func TestCatalogueOverridesAndIndexes(t *testing.T) {
	u := newUPS("ups", "input.quality", "7", "battery.charge", "100",
		"ambient.temperature", "20", "ambient.0.temperature", "21", "ambient.1.temperature", "22")
	cfg := baseCfg()
	cfg.Classify.Numeric = []string{"input.quality"}
	cfg.Classify.Text = []string{"battery.charge"}
	e := newEnv(t, cfg, named{name: "nas", f: newFake(t, u)})
	got := e.scrape()
	expect(t, got, uk("nut_variable_value", "nas", "ups", "variable", "input.quality"), 7)
	expect(t, got, uk("nut_variable_info", "nas", "ups", "variable", "battery.charge", "value", "100"), 1)
	absent(t, got, uk("nut_battery_charge_percent", "nas", "ups"))
	for idx, v := range map[string]float64{"": 20, "0": 21, "1": 22} {
		expect(t, got, uk("nut_ambient_temperature_celsius", "nas", "ups", "ambient", idx), v)
	}
	if n := len(onlyFamilies(got, "nut_suppressed")); n != 0 {
		t.Errorf("suppressions: %v", onlyFamilies(got, "nut_suppressed"))
	}
}
