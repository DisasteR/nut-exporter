// Copyright 2026 Benjamin Saiz
// SPDX-License-Identifier: Apache-2.0

package export_test

import (
	"os"
	"strings"
	"testing"

	"github.com/DisasteR/nut-exporter/internal/fakeupsd"
)

func fixtureUPS(t *testing.T) *upsSpec {
	t.Helper()
	lines, err := fakeupsd.LoadListVar("../../testdata/apc-back-ups-rs-1500g.txt")
	if err != nil {
		t.Fatal(err)
	}
	u := newUPS("ups")
	u.rawVars = lines
	u.logins = 1
	u.clients = []string{"127.0.0.1"}
	u.cmds = []string{"beeper.disable", "beeper.enable", "load.off"}
	return u
}

// Catches: numeric guessing on opaque names (0002, 051d), text values altered (trailing spaces,
// "disabled" turned into a bool), missing/extra dates, zeros for absent flags.
func TestGoldenAPC(t *testing.T) {
	f := newFake(t, fixtureUPS(t))
	e := newEnv(t, baseCfg(), named{name: "nas", f: f})
	got := e.scrape()

	fh, err := os.Open("../../testdata/apc-back-ups-rs-1500g.expected")
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	want := parse(t, fh)
	fams := append(gaugeFamilies(), "nut_variable_info", "nut_variable_timestamp_seconds", "nut_ups_status")
	compare(t, "golden", onlyFamilies(got, fams...), onlyFamilies(want, fams...))
	if n := len(onlyFamilies(want, fams...)); n != 65-3 {
		t.Fatalf("expected file has %d series, hand count is 62", n)
	}

	// Surrounding families (design section 5).
	expect(t, got, k("nut_server_up", "server", "nas"), 1)
	expect(t, got, uk("nut_ups_up", "nas", "ups"), 1)
	expect(t, got, uk("nut_ups_info", "nas", "ups", "description", "desc ups"), 1)
	expect(t, got, uk("nut_ups_logins", "nas", "ups"), 1)
	expect(t, got, uk("nut_ups_client_sessions", "nas", "ups", "address", "127.0.0.1"), 1)
	for _, c := range []string{"beeper.disable", "beeper.enable", "load.off"} {
		expect(t, got, uk("nut_ups_command_info", "nas", "ups", "command", c), 1)
	}
	for _, qn := range []string{"vars", "logins", "clients", "commands", "rw"} {
		expect(t, got, uk("nut_query_success", "nas", "ups", "query", qn), 1)
	}
	if n := len(onlyFamilies(got, "nut_variable_writable")); n != 0 {
		t.Errorf("empty LIST RW exported %d writable series", n)
	}
	if n := len(onlyFamilies(got, "nut_query_error_info")); n != 0 {
		t.Errorf("error series on a healthy UPS: %v", onlyFamilies(got, "nut_query_error_info"))
	}
	// Inventory keeps bytes (design R3) and says how each variable was exported.
	inv := e.inventory()
	if inv["schema"] != float64(1) {
		t.Errorf("schema = %v", inv["schema"])
	}
	u := invUPS(t, inv, "nas", "ups")
	if v := invVar(t, u, "device.serial")["value"]; v != "XX0000X00000  " {
		t.Errorf("device.serial = %q", v)
	}
	if v := invVar(t, u, "ups.productid")["value"]; v != "0002" {
		t.Errorf("ups.productid = %q", v)
	}
	if len(u["variables"].([]any)) != 43 {
		t.Errorf("inventory variables = %d, want 43", len(u["variables"].([]any)))
	}
	// Request log: GET TYPE never sent by the core collection, nothing but the core list.
	for _, r := range f.requests() {
		if strings.HasPrefix(r, "GET TYPE") || strings.HasPrefix(r, "GET DESC") || strings.HasPrefix(r, "GET CMDDESC") ||
			strings.HasPrefix(r, "LIST ENUM") || strings.HasPrefix(r, "LIST RANGE") {
			t.Errorf("core collection sent a metadata request: %q", r)
		}
	}
}

// Catches: a collection that sends GET TYPE (or any metadata request) on the scrape path, a classifier
// driven by TYPE answers, and a core query count different from 3 per server + 5 per UPS.
func TestCoreRequestsOnly(t *testing.T) {
	f := newFake(t, fixtureUPS(t))
	// Every GET TYPE would answer NUMBER (as real upsd does); it must change nothing and never be sent.
	e := newEnv(t, baseCfg(), named{name: "nas", f: f})
	e.scrape()
	e.scrape()
	reqs := f.requests()
	checkRequests(t, reqs)
	for _, r := range reqs {
		if strings.HasPrefix(r, "GET TYPE") {
			t.Fatalf("GET TYPE sent by the core collector: %q", r)
		}
	}
	// Two scrapes, one UPS: LIST UPS, VER, NETVER + 5 per UPS each, no more.
	if want := 2 * (3 + 5); len(reqs) != want {
		t.Errorf("requests = %d, want %d (3 per server + 5 per UPS per scrape): %q", len(reqs), want, reqs)
	}
}
