// Copyright 2026 The nut-exporter Authors
// SPDX-License-Identifier: Apache-2.0

// Package catalog maps NUT variable names of the NUT catalogue to metric families (design §5b).
// The table is generated from NUT's docs/nut-names.txt by ./gen.
package catalog

import (
	"fmt"
	"strings"
)

//go:generate go run ./gen -src $NUT_NAMES -ref $NUT_REF -out catalog_gen.go

// Family is one catalogue-named metric. Patterns are NUT names where `{label}` stands for an
// index or a three-phase context, exported as the label of that name.
type Family struct {
	Name, Unit, Label string
	Patterns          []string
}

// Excluded is a catalogue pattern left to the generic families, with the reason.
type Excluded struct{ Pattern, Reason string }

// Match is the family of a NUT variable and the value of its label ("" for an unindexed name).
type Match struct {
	Family     *Family
	LabelValue string
}

var (
	byPattern = map[string]*Family{}
	indexed   = []struct{ prefix, label string }{
		{"outlet.group.", "outlet_group"}, {"ambient.", "ambient"}, {"outlet.", "outlet"},
	}
	domains = []string{"input.servicebypass", "input.bypass", "input", "output.servicebypass",
		"output.bypass", "output.inverter", "output"} // longest first
	contexts = map[string]bool{"L1-L2": true, "L2-L3": true, "L3-L1": true, "L1-N": true,
		"L2-N": true, "L3-N": true, "L1": true, "L2": true, "L3": true, "N": true}
)

func init() {
	for i := range families {
		for _, p := range families[i].Patterns {
			byPattern[p] = &families[i]
		}
	}
}

// Families returns the catalogue families.
func Families() []Family { return families }

// Help is the HELP text of a family: NUT names and the unit the catalogue declares (not checked),
// no NUT prose.
func (f *Family) Help() string {
	h := "NUT " + strings.Join(f.Patterns, ", ")
	if f.Unit != "" {
		h += " (catalogue unit: " + f.Unit + ")"
	}
	return h + "."
}

// Lookup returns the family of a NUT variable name.
func Lookup(name string) (Match, bool) {
	if strings.ContainsAny(name, "{}") {
		return Match{}, false // never a pattern's own text: the mapping stays injective
	}
	if f, ok := byPattern[name]; ok {
		return Match{Family: f}, true
	}
	for _, ix := range indexed {
		rest, ok := strings.CutPrefix(name, ix.prefix)
		if !ok {
			continue
		}
		n, tail, ok := strings.Cut(rest, ".")
		if ok && tail != "" && digits(n) {
			if f, ok := byPattern[fmt.Sprintf("%s{%s}.%s", ix.prefix, ix.label, tail)]; ok {
				return Match{Family: f, LabelValue: n}, true
			}
		}
	}
	for _, d := range domains {
		rest, ok := strings.CutPrefix(name, d+".")
		if !ok {
			continue
		}
		ctx, tail, ok := strings.Cut(rest, ".")
		if ok && tail != "" && contexts[ctx] {
			if f, ok := byPattern[d+".{phase}."+tail]; ok {
				return Match{Family: f, LabelValue: ctx}, true
			}
		}
	}
	return Match{}, false
}

func digits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
