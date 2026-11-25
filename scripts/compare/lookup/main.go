// Copyright 2026 Benjamin Saiz
// SPDX-License-Identifier: Apache-2.0

// Command lookup writes, for each variable name of a fakesrv index, the catalogue family of
// nut-exporter and its label (name, value), so that analyze.py can find the series.
package main

import (
	"encoding/json"
	"log"
	"os"

	"github.com/DisasteR/nut-exporter/internal/catalog"
)

func main() {
	if len(os.Args) != 3 {
		log.Fatal("usage: lookup INDEX OUT")
	}
	b, err := os.ReadFile(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	var dumps []struct{ Vars []struct{ Name string } }
	if err := json.Unmarshal(b, &dumps); err != nil {
		log.Fatal(err)
	}
	out := map[string][3]string{}
	for _, d := range dumps {
		for _, v := range d.Vars {
			if m, ok := catalog.Lookup(v.Name); ok {
				out[v.Name] = [3]string{m.Family.Name, m.Family.Label, m.LabelValue}
			}
		}
	}
	if b, err = json.Marshal(out); err == nil {
		err = os.WriteFile(os.Args[2], b, 0o644)
	}
	if err != nil {
		log.Fatal(err)
	}
}
