// Copyright 2026 Benjamin Saiz
// SPDX-License-Identifier: Apache-2.0

package export_test

import (
	"bufio"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/DisasteR/nut-exporter/internal/catalog"
)

// ddlLine is a `name: value` line of a NUT DDL dump; the value is kept verbatim (upsc prints it so).
var (
	ddlLine    = regexp.MustCompile(`^([A-Za-z0-9._-]+):(?: (.*))?$`)
	ddlComment = regexp.MustCompile(`[ \t]+#.*$`)
)

// TestDDLCorpus replays every dump of NUT's Devices Dumps Library (NUT_DDL, a nut-ddl checkout)
// through a fake upsd and the whole exporter, and writes one JSON line per dump to NUT_DDL_OUT.
// Catches: a dump whose variables are not all collected, kept verbatim in /inventory and exported
// (or reported as suppressed) under the metric the inventory names.
func TestDDLCorpus(t *testing.T) {
	root, out := os.Getenv("NUT_DDL"), os.Getenv("NUT_DDL_OUT")
	if root == "" || out == "" {
		t.Skip("NUT_DDL and NUT_DDL_OUT not set")
	}
	w, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	enc := json.NewEncoder(w)
	dumps := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".dev") && !strings.HasSuffix(path, ".nds") {
			return err
		}
		kv, err := readDump(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		dumps++
		t.Run(rel, func(t *testing.T) {
			e := newEnv(t, baseCfg(), named{name: "nas", f: newFake(t, newUPS("ups", kv...))})
			got := e.scrape()
			sup := map[string]float64{}
			for key, v := range onlyFamilies(got, "nut_suppressed") {
				sup[key] = v
			}
			expect(t, got, uk("nut_query_success", "nas", "ups", "query", "vars"), 1)
			u := invUPS(t, e.inventory(), "nas", "ups")
			checkDump(t, got, u, kv)
			if err := enc.Encode(map[string]any{"file": rel, "series": len(got), "suppressed": sup,
				"complete": u["complete"], "variables": u["variables"]}); err != nil {
				t.Fatal(err)
			}
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if dumps == 0 {
		t.Fatalf("no .dev or .nds file under %s", root)
	}
}

// checkDump compares the inventory with the dump (last value wins) and finds each variable's series.
func checkDump(t *testing.T, got samples, u map[string]any, kv []string) {
	t.Helper()
	want := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		want[kv[i]] = kv[i+1]
	}
	vars, _ := u["variables"].([]any)
	if len(vars) != len(want) {
		t.Errorf("inventory has %d variables, dump %d", len(vars), len(want))
	}
	for _, x := range vars {
		v := x.(map[string]any)
		name, _ := v["name"].(string)
		value, ok := v["value"].(string)
		if w, in := want[name]; !in || !ok || value != w {
			t.Errorf("%s: inventory value %q, dump %q (listed %v)", name, value, w, in)
			continue
		}
		if v["suppressed"] != nil {
			continue
		}
		metric, _ := v["metric"].(string)
		var key string
		switch m, _ := catalog.Lookup(name); {
		case v["export"] == "info":
			key = uk("nut_variable_info", "nas", "ups", "variable", name, "value", value)
		case metric == "nut_variable_value":
			key = uk(metric, "nas", "ups", "variable", name)
		case m.Family != nil && m.Family.Name == metric && m.Family.Label != "":
			key = uk(metric, "nas", "ups", m.Family.Label, m.LabelValue)
		default:
			key = uk(metric, "nas", "ups")
		}
		if _, ok := got[key]; !ok {
			t.Errorf("%s=%q: no series %s", name, value, key)
		}
	}
}

// readDump returns name/value pairs; comments, RW: and CMD: lines are skipped.
func readDump(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var kv []string
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimRight(s.Text(), "\r")
		line = ddlComment.ReplaceAllString(line, "") // dummy-ups: # after a blank starts a comment
		if m := ddlLine.FindStringSubmatch(line); m != nil {
			kv = append(kv, m[1], m[2])
		}
	}
	return kv, s.Err()
}
