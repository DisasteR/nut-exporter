package export_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/DisasteR/nut-exporter/internal/fakeupsd"
)

// Catches: churn computed on the size of a family instead of its turnover, which wipes the
// commands and writable families of a PDU-class device on the first scrape.
func TestLargeStaticFamiliesAreNotChurn(t *testing.T) {
	a := newUPS("a", "battery.charge", "100")
	for i := 0; i < 150; i++ {
		a.cmds = append(a.cmds, fmt.Sprintf("outlet.%d.load.off", i))
		a.rw = append(a.rw, rwLine("a", fmt.Sprintf("outlet.%d.delay.start", i), "0"))
		a.vars = append(a.vars, varLine("a", fmt.Sprintf("outlet.%d.delay.start", i), "0"))
	}
	e := newEnv(t, baseCfg(), named{name: "nas", f: newFake(t, a)})
	for scrape := 0; scrape < 2; scrape++ {
		got := e.scrape()
		if n := len(withLabel(onlyFamilies(got, "nut_ups_command_info"), "ups", "a")); n != 150 {
			t.Fatalf("scrape %d: %d command series, want 150", scrape, n)
		}
		if n := len(withLabel(onlyFamilies(got, "nut_variable_writable"), "ups", "a")); n != 150 {
			t.Fatalf("scrape %d: %d writable series, want 150", scrape, n)
		}
		for key := range onlyFamilies(got, "nut_suppressed") {
			if strings.Contains(key, `reason="churn"`) {
				t.Fatalf("scrape %d: %s", scrape, key)
			}
		}
	}
}

// Catches: a repeated name in one LIST answer producing duplicate series, which makes the
// registry fail the whole /metrics answer and hides every other UPS.
func TestDuplicateNamesDoNotBreakMetrics(t *testing.T) {
	a, b := common("a"), common("b")
	a.vars = append(a.vars, varLine("a", "battery.charge", "50"))
	a.rw = append(a.rw, rwLine("a", "battery.charge", "50"), rwLine("a", "battery.charge", "50"))
	f := newFake(t, a, b)
	e := newEnv(t, baseCfg(), named{name: "nas", f: f})
	rec := e.serve(context.Background(), "/metrics", e.exp.ServeMetrics)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %.300s", rec.Code, rec.Body.String())
	}
	got := parse(t, rec.Body)
	expect(t, got, uk("nut_battery_charge_percent", "nas", "b"), 100)
	expect(t, got, uk("nut_battery_charge_percent", "nas", "a"), 50) // last wins
	expect(t, got, uk("nut_suppressed", "nas", "a", "family", "nut_battery_charge_percent", "reason", "duplicate"), 1)
	expect(t, got, uk("nut_variable_writable", "nas", "a", "variable", "battery.charge"), 1)
}

// Catches: filtered variables vanishing silently from the writable family.
func TestFilteredWritableIsCounted(t *testing.T) {
	a := common("a")
	a.rw = []string{rwLine("a", "battery.charge", "100")}
	cfg := baseCfg()
	cfg.Variables.Exclude = []string{"battery.*"}
	e := newEnv(t, cfg, named{name: "nas", f: newFake(t, a)})
	got := e.scrape()
	absent(t, got, uk("nut_variable_writable", "nas", "a", "variable", "battery.charge"))
	expect(t, got, uk("nut_suppressed", "nas", "a", "family", "nut_variable_writable", "reason", "filter"), 1)
}

// Catches: an inventory that reports a variable as exported while a guard hid it.
func TestInventoryReportsSuppression(t *testing.T) {
	a := newUPS("a", "device.description", strings.Repeat("x", 300), "device.model", "m")
	cfg := baseCfg()
	cfg.Guard.MaxLength = 100
	e := newEnv(t, cfg, named{name: "nas", f: newFake(t, a)})
	e.scrape()
	u := invUPS(t, e.inventory(), "nas", "a")
	if got := invVar(t, u, "device.description")["suppressed"]; got != "length" {
		t.Fatalf("device.description suppressed = %v, want length", got)
	}
	if got, ok := invVar(t, u, "device.model")["suppressed"]; ok && got != "" {
		t.Fatalf("device.model suppressed = %v, want none", got)
	}
}

// Catches: arbitrary server error text used as a label, i.e. unbounded cardinality.
func TestUnknownErrorWordIsBounded(t *testing.T) {
	a := common("a")
	a.override = map[string]fakeupsd.Response{"LIST CLIENT a": fakeErr("ERR SOMETHING-NEW-1234")}
	e := newEnv(t, baseCfg(), named{name: "nas", f: newFake(t, a)})
	got := e.scrape()
	expect(t, got, uk("nut_query_error_info", "nas", "a", "query", "clients", "error", "other"), 1)
}

func rwLine(ups, name, value string) string { return "RW " + ups + " " + name + " " + nutq(value) }
