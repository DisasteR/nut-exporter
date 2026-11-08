package catalog

import (
	"regexp"
	"strings"
	"testing"
)

// Catches: wrong metric name, unit or label for standard names, indexes and three-phase contexts.
func TestLookup(t *testing.T) {
	for name, want := range map[string][2]string{
		"battery.charge":            {"nut_battery_charge_percent", ""},
		"battery.runtime":           {"nut_battery_runtime_seconds", ""},
		"ups.realpower.nominal":     {"nut_ups_realpower_nominal_watts", ""},
		"ups.timer.reboot":          {"nut_ups_timer_reboot_seconds", ""}, // unit on a continuation line
		"battery.voltage.low":       {"nut_battery_voltage_low", ""},      // no unit stated by NUT
		"input.voltage":             {"nut_input_voltage_volts", ""},
		"input.L1-N.voltage":        {"nut_input_voltage_volts", "L1-N"},
		"input.bypass.L2.current":   {"nut_input_bypass_current_amperes", "L2"},
		"output.L3.realpower":       {"nut_output_realpower_watts", "L3"},
		"input.frequency":           {"nut_input_frequency_hertz", ""},
		"ambient.1.temperature":     {"nut_ambient_temperature_celsius", "1"},
		"ambient.01.temperature":    {"nut_ambient_temperature_celsius", "01"},
		"outlet.3.current":          {"nut_outlet_current_amperes", "3"},
		"outlet.group.2.current":    {"nut_outlet_group_current_amperes", "2"},
		"battery.capacity":          {"nut_battery_capacity_ampere_hours", ""},
		"ups.temperature":           {"nut_ups_temperature_celsius", ""},
		"input.transfer.boost.high": {"nut_input_transfer_boost_high_volts", ""},
		"ambient.temperature":       {"nut_ambient_temperature_celsius", ""}, // unindexed = ambient.0
		"outlet.current":            {"nut_outlet_current_amperes", ""},
		"input.load":                {"nut_input_load_percent", ""},
		"input.L2.load":             {"nut_input_load_percent", "L2"},  // spec states no unit
		"input.phases":              {"nut_input_phases", ""},          // defined in prose
		"device.uptime":             {"nut_device_uptime_seconds", ""}, // unit stated in the sentence
		"outlet.switchable":         {"nut_outlet_switchable", ""},     // outlet = outlet.0
		"input.L1.power.percent":    {"nut_input_power_percent", "L1"},
	} {
		m, ok := Lookup(name)
		if !ok {
			t.Errorf("%s: not in the catalogue", name)
			continue
		}
		if m.Family.Name != want[0] || m.LabelValue != want[1] {
			t.Errorf("%s: %s{%s=%q}, want %s %q", name, m.Family.Name, m.Family.Label, m.LabelValue, want[0], want[1])
		}
	}
	for _, name := range []string{"experimental.foo", "input.L4.voltage", "ambient.x.temperature",
		"outlet.1", "input.L1", "ambient..temperature", "battery.charge.foo", "ups.status", "server.info",
		"battery.charge.approx", "input.quality", "driver.parameter.xxx",
		"input.{phase}.voltage", "outlet.{outlet}.voltage", // a pattern's own text
		"outlet.group.count", // groups count, not outlets of a group: generic
		"input.L1.frequency", // frequency is aggregate-only in the catalogue
	} {
		if m, ok := Lookup(name); ok {
			t.Errorf("%s: matched %s", name, m.Family.Name)
		}
	}
}

// Catches: a generated table that would give invalid, duplicate or ambiguous metrics.
func TestFamiliesInvariants(t *testing.T) {
	valid := regexp.MustCompile(`^nut_[a-z0-9_]+$`)
	placeholder := regexp.MustCompile(`\{([a-z_]+)\}`)
	names := map[string]bool{}
	patterns := map[string]bool{}
	for _, f := range Families() {
		if !valid.MatchString(f.Name) || strings.HasPrefix(f.Name, "nut_variable_") {
			t.Errorf("invalid name %s", f.Name)
		}
		if names[f.Name] {
			t.Errorf("duplicate family %s", f.Name)
		}
		names[f.Name] = true
		for _, p := range f.Patterns {
			if patterns[p] {
				t.Errorf("pattern %s in two families", p)
			}
			patterns[p] = true
			for _, m := range placeholder.FindAllStringSubmatch(p, -1) {
				if m[1] != f.Label {
					t.Errorf("%s: placeholder %s, family label %q", p, m[1], f.Label)
				}
			}
			// Lookup must give back this family for a concrete instance of the pattern.
			concrete := strings.NewReplacer("{phase}", "L1", "{ambient}", "7", "{outlet}", "7", "{outlet_group}", "7").Replace(p)
			want := ""
			if concrete != p {
				want = map[string]string{"phase": "L1", "ambient": "7", "outlet": "7", "outlet_group": "7"}[f.Label]
			}
			if m, ok := Lookup(concrete); !ok || m.Family.Name != f.Name || m.LabelValue != want {
				t.Errorf("%s (%s): lookup gives %+v %v, want %s %q", p, concrete, m, ok, f.Name, want)
			}
		}
	}
	// Changes only with a deliberate regeneration (metric names are a contract).
	if len(names) != 421 || len(Text) != 50 {
		t.Errorf("%d families, %d text patterns: regenerated catalogue, update deliberately", len(names), len(Text))
	}
}
