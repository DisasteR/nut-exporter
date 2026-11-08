// Copyright 2026 Benjamin Saiz
// SPDX-License-Identifier: Apache-2.0

// Command gen writes internal/catalog/catalog_gen.go from NUT's docs/nut-names.txt (design §5b).
//
//	go run ./internal/catalog/gen -src path/to/nut/docs/nut-names.txt -ref <NUT commit>
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"go/format"
	"log"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Indexed collections: prefix -> label (design §5b).
var indexed = []struct{ prefix, label string }{
	{"outlet.group.", "outlet_group"}, {"ambient.", "ambient"}, {"outlet.", "outlet"},
}

// Three-phase domains (nut-names.txt "DOMAINs").
var domains = []string{"input", "input.bypass", "input.servicebypass", "output", "output.bypass",
	"output.inverter", "output.servicebypass"}

// notUnits are final parentheses of the catalogue that state no unit. Any other unknown text
// fails the generation, so a new unit in a later catalogue is reviewed, never dropped silently.
var notUnits = map[string]bool{
	"dimensionless, equal to or greater than 1": true, "dimensionless value between 0.00 and 1.00": true,
	"dimensionless, value between 0 and 1": true, "dimensionless value greater or equal to 1": true,
	"enabled/disabled": true, "enabled, disabled or muted": true, "enabled or disabled": true,
	"i.e. battery.charge = 100": true, "NUT release": true, "on/off": true, "poweroff": true,
	"see the note below": true, "ups, pdu, scd, psu, ats": true, "yes/no": true,
}

var unknownUnits = map[string]bool{}

// wordEntries lists the entries made text by their example, logged for review.
var wordEntries []string

// unit maps the unit stated in the last parentheses of a description to a metric suffix.
func unit(desc string) string {
	m := regexp.MustCompile(`\(([^()]*)\)\s*$`).FindStringSubmatch(desc)
	if m == nil {
		// A unit stated in the sentence: "Device uptime in seconds".
		if regexp.MustCompile(`\bin seconds$`).MatchString(desc) {
			return "seconds"
		}
		return ""
	}
	u := strings.Join(strings.Fields(m[1]), " ")
	switch {
	case u == "V":
		return "volts"
	case u == "A":
		return "amperes"
	case u == "percent", strings.HasPrefix(u, "percent of "):
		return "percent"
	case u == "seconds", strings.HasPrefix(u, "seconds,"):
		return "seconds"
	case u == "degrees C":
		return "celsius"
	case u == "degrees":
		return "degrees"
	case u == "Hz":
		return "hertz"
	case u == "VA", u == "Volt-Amps":
		return "voltamperes"
	case u == "W", u == "Watts":
		return "watts"
	case u == "Ah":
		return "ampere_hours"
	case u == "min":
		return "minutes"
	}
	if !notUnits[u] && !opaque(desc) {
		unknownUnits[u] = true
	}
	return ""
}

// opaque reports a description the catalogue marks as an opaque string: always text.
func opaque(desc string) bool { return strings.Contains(strings.ToLower(desc), "opaque") }

// wordValued reports an entry whose catalogue example is a word, not a number: an enum such as
// yes/no or enabled/disabled, with or without the values in its description. The example may
// carry a comment ("0 (immediate)") or a decimal comma ("21,52"): still a number.
func wordValued(desc, example string) bool {
	return example != "" && unit(desc) == "" && !regexp.MustCompile(`^-?[0-9]+([.,][0-9]+)?(\s|$)`).MatchString(example)
}

// reserved are the fixed family names of internal/export; a catalogue name equal to one of them,
// or under nut_variable_, is left to the generic families.
var reserved = map[string]bool{
	"nut_server_up": true, "nut_server_info": true, "nut_server_query_success": true,
	"nut_server_suppressed": true, "nut_collect_duration_seconds": true, "nut_ups_info": true,
	"nut_ups_up": true, "nut_query_success": true, "nut_query_error_info": true,
	"nut_metadata_age_seconds": true, "nut_metadata_errors": true, "nut_metadata_pending": true,
	"nut_suppressed": true, "nut_ups_logins": true, "nut_ups_status": true,
	"nut_ups_client_sessions": true, "nut_ups_command_info": true, "nut_exporter_build_info": true,
}

type row struct{ name, desc, example string }

// parse returns the variable rows and the two three-phase SPEC tables.
func parse(src []byte) (vars []row, specCtx, specAgg []row) {
	var (
		lines   []string
		heading string
		inTable bool
		table   []row
		tableNo int // index of the table under the current heading
	)
	sc := bufio.NewScanner(bytes.NewReader(src))
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	underline := regexp.MustCompile(`^(~{3,}|\^{3,}|-{3,})$`)
	flush := func() {
		switch {
		case heading == "Valid SPECs" && tableNo == 0:
			specCtx = table
		case heading == "Valid SPECs" && tableNo == 1:
			specAgg = table
		case strings.Contains(strings.ToLower(heading), "instant commands"):
		default:
			vars = append(vars, table...)
		}
		table = nil
		tableNo++
	}
	for i, l := range lines {
		if !inTable && underline.MatchString(l) && i > 0 && strings.TrimSpace(lines[i-1]) != "" {
			heading, tableNo = strings.TrimSpace(lines[i-1]), 0
			continue
		}
		if strings.HasPrefix(l, "|===") {
			if inTable {
				flush()
			}
			inTable = !inTable
			continue
		}
		if !inTable {
			continue
		}
		if strings.HasPrefix(l, "|") {
			cells := strings.Split(l[1:], "|")
			table = append(table, row{name: strings.TrimSpace(cells[0])})
			if len(cells) > 1 {
				table[len(table)-1].desc = strings.TrimSpace(cells[1])
			}
			if len(cells) > 2 {
				table[len(table)-1].example = strings.TrimSpace(cells[2])
			}
			continue
		}
		// Continuation line: aligned under the description column, example after a `|`.
		if len(table) > 0 && strings.TrimSpace(l) != "" {
			r := &table[len(table)-1]
			d, ex, _ := strings.Cut(l, "|")
			r.desc = strings.TrimSpace(r.desc + " " + strings.TrimSpace(d))
			r.example = strings.TrimSpace(r.example + " " + strings.TrimSpace(ex))
		}
	}
	return vars, specCtx, specAgg
}

type family struct {
	Name, Unit, Label string
	Patterns          []string
}

type excluded struct{ Pattern, Reason string }

func metricName(pattern, unit string) string {
	base := pattern
	for _, ix := range indexed {
		base = strings.Replace(base, "{"+ix.label+"}.", "", 1)
	}
	base = strings.Replace(base, "{phase}.", "", 1)
	n := "nut_" + strings.NewReplacer(".", "_", "-", "_").Replace(base)
	if unit != "" && !strings.HasSuffix(n, "_"+unit) {
		n += "_" + unit
	}
	return n
}

// prose lists numeric names the catalogue defines in prose only, with their unit.
var prose = []struct{ pattern, unit string }{
	{"input.phases", ""}, {"output.phases", ""}, // "Phase Count Determination"
}

type entry struct {
	pattern, unit, label string
	text                 bool // the catalogue marks it opaque or its example is a word
	joins                bool // unindexed form NUT defines as equivalent to its indexed family
}

// base is a pattern without its index or context placeholder.
func base(p string) string {
	return regexp.MustCompile(`\{[a-z_]+\}\.`).ReplaceAllString(p, "")
}

func build(vars, specCtx, specAgg []row) ([]family, []excluded, []string, error) {
	name := regexp.MustCompile(`^[a-z][a-z0-9]*(\.[A-Za-z0-9-]+)+$`)
	placeholder := regexp.MustCompile(`\.(x|xxx)(\.|$)`)
	var entries []entry
	wordEntries = nil
	for _, r := range vars {
		if !name.MatchString(r.name) || placeholder.MatchString(r.name) {
			continue // header rows, table notes, `x`/`xxx` placeholders (text only)
		}
		p, label := r.name, ""
		for _, ix := range indexed {
			if strings.HasPrefix(p, ix.prefix+"n.") {
				p, label = ix.prefix+"{"+ix.label+"}."+strings.TrimPrefix(p, ix.prefix+"n."), ix.label
				break
			}
		}
		// NUT: "ambient.0" is equivalent to "ambient", "outlet.0" to "outlet" (without index).
		eq := label == "" && (strings.HasPrefix(p, "ambient.") || strings.HasPrefix(p, "outlet.")) &&
			!strings.HasPrefix(p, "outlet.group.")
		words := wordValued(r.desc, r.example) && !opaque(r.desc)
		if words {
			wordEntries = append(wordEntries, r.name+"="+r.example)
		}
		entries = append(entries, entry{pattern: p, unit: unit(r.desc), label: label, text: opaque(r.desc) || words, joins: eq})
	}
	for _, p := range prose {
		entries = append(entries, entry{pattern: p.pattern, unit: p.unit})
	}
	defined := map[string]bool{}
	for _, e := range entries {
		defined[e.pattern] = true
	}
	for _, e := range append([]entry(nil), entries...) {
		// "The same principles and data than the outlet collection apply to outlet.group".
		if rest, ok := strings.CutPrefix(e.pattern, "outlet.{outlet}."); ok {
			if g := "outlet.group.{outlet_group}." + rest; !defined[g] {
				entries = append(entries, entry{g, e.unit, "outlet_group", e.text, false})
				defined[g] = true
			}
		}
		// "ambient.0" is equivalent to "ambient" (without index); outlet likewise: drivers
		// publish both forms. The unindexed form joins the family with an empty label.
		for _, ix := range []string{"ambient", "outlet"} {
			if rest, ok := strings.CutPrefix(e.pattern, ix+".{"+ix+"}."); ok {
				if u := ix + "." + rest; !defined[u] {
					entries = append(entries, entry{u, e.unit, "", e.text, true})
					defined[u] = true
				}
			}
		}
	}
	for _, d := range domains {
		for _, s := range specCtx {
			if s.name == "Name" {
				continue
			}
			entries = append(entries, entry{d + ".{phase}." + s.name, unit(s.desc), "phase", opaque(s.desc), false},
				entry{d + "." + s.name, unit(s.desc), "", opaque(s.desc), true})
		}
		for _, s := range specAgg {
			if s.name == "Name" {
				continue
			}
			entries = append(entries, entry{d + "." + s.name, unit(s.desc), "", opaque(s.desc), true})
		}
	}
	if len(unknownUnits) > 0 {
		var u []string
		for k := range unknownUnits {
			u = append(u, k)
		}
		sort.Strings(u)
		return nil, nil, nil, fmt.Errorf("unknown final parentheses, add to the unit map or notUnits: %q", u)
	}
	// One quantity, one unit: an entry stating none takes the unit its base name states
	// elsewhere (spec `load` and `input.load` "percent of full").
	units := map[string]map[string]bool{}
	text := map[string]bool{}
	for _, e := range entries {
		if e.text {
			text[base(e.pattern)] = true
		}
		if e.unit != "" {
			if units[base(e.pattern)] == nil {
				units[base(e.pattern)] = map[string]bool{}
			}
			units[base(e.pattern)][e.unit] = true
		}
	}
	for i, e := range entries {
		if us := units[base(e.pattern)]; e.unit == "" && len(us) == 1 {
			for u := range us {
				entries[i].unit = u
			}
		}
		if entries[i].unit == "" && strings.HasSuffix(e.pattern, ".percent") {
			entries[i].unit = "percent" // the name states it (power.percent)
		}
	}
	// A literal joins an indexed family only as a documented equivalent (DOMAIN.SPEC, ambient.X,
	// outlet.X); a table literal that would merge with one is another quantity (DOMAIN.CONTEXT
	// literals such as input.voltage are DOMAIN.SPEC and join).
	indexedNames := map[string]bool{}
	for _, e := range entries {
		if e.label != "" {
			indexedNames[metricName(e.pattern, e.unit)] = true
		}
	}
	joinable := map[string]bool{}
	for _, e := range entries {
		if e.joins {
			joinable[e.pattern] = true
		}
	}
	for _, e := range entries {
		if e.label == "" && !e.text && indexedNames[metricName(e.pattern, e.unit)] && !joinable[e.pattern] {
			return nil, nil, nil, fmt.Errorf("%s would merge with an indexed family of another quantity", e.pattern)
		}
	}
	byName := map[string]*family{}
	seen := map[string]bool{}
	var excl []excluded
	var textGlobs []string
	for _, e := range entries {
		if e.text || text[base(e.pattern)] {
			if g := regexp.MustCompile(`\{[a-z_]+\}`).ReplaceAllString(e.pattern, "*"); !seen["text "+g] {
				seen["text "+g] = true
				textGlobs = append(textGlobs, g)
			}
			continue
		}
		n := metricName(e.pattern, e.unit)
		if reserved[n] || strings.HasPrefix(n, "nut_variable_") {
			excl = append(excl, excluded{e.pattern, "reserved name " + n})
			continue
		}
		f := byName[n]
		if f == nil {
			f = &family{Name: n, Unit: e.unit}
			byName[n] = f
		}
		if f.Unit != e.unit {
			return nil, nil, nil, fmt.Errorf("%s: unit %q and %q", n, f.Unit, e.unit)
		}
		switch {
		case e.label == "" || f.Label == e.label:
		case f.Label == "":
			f.Label = e.label
		default:
			return nil, nil, nil, fmt.Errorf("%s: labels %q and %q", n, f.Label, e.label)
		}
		if !seen[e.pattern] {
			seen[e.pattern] = true
			f.Patterns = append(f.Patterns, e.pattern)
		}
	}
	// A pattern must belong to one family only.
	owner := map[string]string{}
	var out []family
	for _, f := range byName {
		for _, p := range f.Patterns {
			if o, ok := owner[p]; ok {
				return nil, nil, nil, fmt.Errorf("pattern %s in %s and %s", p, o, f.Name)
			}
			owner[p] = f.Name
		}
		sort.Strings(f.Patterns)
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	sort.Strings(textGlobs)
	return out, excl, textGlobs, nil
}

func main() {
	src := flag.String("src", "", "NUT docs/nut-names.txt")
	ref := flag.String("ref", "", "NUT commit the file comes from")
	out := flag.String("out", "internal/catalog/catalog_gen.go", "output file")
	flag.Parse()
	if *src == "" || *ref == "" {
		log.Fatal("-src and -ref are required")
	}
	b, err := os.ReadFile(*src)
	if err != nil {
		log.Fatal(err)
	}
	vars, specCtx, specAgg := parse(b)
	if len(specCtx) == 0 || len(specAgg) == 0 {
		log.Fatal("three-phase SPEC tables not found")
	}
	fams, excl, text, err := build(vars, specCtx, specAgg)
	if err != nil {
		log.Fatal(err)
	}
	var w bytes.Buffer
	fmt.Fprintf(&w, "// Copyright 2026 Benjamin Saiz\n// SPDX-License-Identifier: Apache-2.0\n\n")
	fmt.Fprintf(&w, "// Code generated by go run ./internal/catalog/gen; DO NOT EDIT.\n")
	fmt.Fprintf(&w, "// Source: NUT docs/nut-names.txt at %s.\n\npackage catalog\n\n", *ref)
	fmt.Fprintf(&w, "// Source is the NUT commit of the catalogue.\nconst Source = %q\n\n", *ref)
	fmt.Fprintf(&w, "var families = []Family{\n")
	for _, f := range fams {
		fmt.Fprintf(&w, "\t{Name: %q, Unit: %q, Label: %q, Patterns: %#v},\n", f.Name, f.Unit, f.Label, f.Patterns)
	}
	fmt.Fprintf(&w, "}\n\n// excluded are catalogue patterns left to the generic families.\nvar excluded = []Excluded{\n")
	for _, e := range excl {
		fmt.Fprintf(&w, "\t{Pattern: %q, Reason: %q},\n", e.Pattern, e.Reason)
	}
	fmt.Fprintf(&w, "}\n\n// Text are the names the catalogue marks opaque, as path.Match patterns: always text.\nvar Text = %#v\n", text)
	code, err := format.Source(w.Bytes())
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*out, code, 0o644); err != nil {
		log.Fatal(err)
	}
	log.Printf("text by example (review): %s", strings.Join(wordEntries, ", "))
	log.Printf("%d families, %d excluded, %d opaque", len(fams), len(excl), len(text))
}
