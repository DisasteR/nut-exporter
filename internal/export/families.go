// Package export projects collection snapshots into Prometheus metrics and the inventory
// (design §5, §6, §6b).
package export

import (
	"github.com/DisasteR/nut-exporter/internal/catalog"
	"github.com/prometheus/client_golang/prometheus"
)

type family struct {
	name   string
	help   string
	labels []string // after the scope labels
	scope  int      // scopeServer: server; scopeUPS: server, ups
	kind   prometheus.ValueType
	budget int // drop priority under max_series; 0 = outside the budget
}

const (
	scopeServer = iota
	scopeUPS
)

// Drop priorities under max_series, lowest dropped first (design §6). Variables come last and
// are dropped in reverse name order.
const (
	prioEnum = iota + 1
	prioRange
	prioType
	prioWritable
	prioCommands
	prioClients
	prioTimestamps
	prioVariables
)

var (
	fServerUp         = &family{name: "nut_server_up", help: "1 if LIST UPS succeeded.", scope: scopeServer}
	fServerInfo       = &family{name: "nut_server_info", help: "Raw VER and NETVER answers.", labels: []string{"version", "protocol"}, scope: scopeServer}
	fServerQuery      = &family{name: "nut_server_query_success", help: "1 if the server query succeeded.", labels: []string{"query"}, scope: scopeServer}
	fServerSuppressed = &family{name: "nut_server_suppressed", help: "UPSes not exported, by reason (filter, ups_cap, encoding, duplicate).", labels: []string{"reason"}, scope: scopeServer}
	fDuration         = &family{name: "nut_collect_duration_seconds", help: "Duration of the last collection of the server.", scope: scopeServer}

	fUPSInfo     = &family{name: "nut_ups_info", help: "UPS listed by upsd, with its description.", labels: []string{"description"}, scope: scopeUPS}
	fUPSUp       = &family{name: "nut_ups_up", help: "1 if LIST VAR succeeded.", scope: scopeUPS}
	fQuery       = &family{name: "nut_query_success", help: "1 if the core query succeeded.", labels: []string{"query"}, scope: scopeUPS}
	fQueryError  = &family{name: "nut_query_error_info", help: "Error of a failed core query.", labels: []string{"query", "error"}, scope: scopeUPS}
	fMetaAge     = &family{name: "nut_metadata_age_seconds", help: "Age of the oldest successful metadata entry.", labels: []string{"query"}, scope: scopeUPS}
	fMetaErrors  = &family{name: "nut_metadata_errors", help: "Metadata entries whose last refresh failed.", labels: []string{"query"}, scope: scopeUPS}
	fMetaPending = &family{name: "nut_metadata_pending", help: "Metadata entries never refreshed yet.", labels: []string{"query"}, scope: scopeUPS}
	fSuppressed  = &family{name: "nut_suppressed", help: "Series not exported this scrape, by family and reason.", labels: []string{"family", "reason"}, scope: scopeUPS}
	fParseErrors = &family{name: "nut_variable_parse_errors_total", help: "Values of a variable forced numeric that did not parse.", labels: []string{"variable"}, scope: scopeUPS, kind: prometheus.CounterValue}
	fLogins      = &family{name: "nut_ups_logins", help: "Sessions logged in to the UPS (GET NUMLOGINS).", scope: scopeUPS}
	fValue       = &family{name: "nut_variable_value", help: "Numeric NUT variable outside the NUT catalogue, in NUT units.", labels: []string{"variable"}, scope: scopeUPS, budget: prioVariables}
	fInfo        = &family{name: "nut_variable_info", help: "Text NUT variable, value verbatim.", labels: []string{"variable", "value"}, scope: scopeUPS, budget: prioVariables}
	fStatus      = &family{name: "nut_ups_status", help: "ups.status flags; only when ups.status was read.", labels: []string{"flag"}, scope: scopeUPS, budget: prioVariables}
	fTimestamp   = &family{name: "nut_variable_timestamp_seconds", help: "Date variable as Unix time (00:00 UTC).", labels: []string{"variable"}, scope: scopeUPS, budget: prioTimestamps}
	fClients     = &family{name: "nut_ups_client_sessions", help: "Logged-in sessions per address (LIST CLIENT).", labels: []string{"address"}, scope: scopeUPS, budget: prioClients}
	fCommands    = &family{name: "nut_ups_command_info", help: "Instant command supported by the UPS.", labels: []string{"command"}, scope: scopeUPS, budget: prioCommands}
	fWritable    = &family{name: "nut_variable_writable", help: "Writable variable (LIST RW).", labels: []string{"variable"}, scope: scopeUPS, budget: prioWritable}
	fType        = &family{name: "nut_variable_type_info", help: "GET TYPE word of a variable; not a reliable type.", labels: []string{"variable", "type"}, scope: scopeUPS, budget: prioType}
	fEnum        = &family{name: "nut_variable_enum_info", help: "Allowed value of a variable (LIST ENUM).", labels: []string{"variable", "value"}, scope: scopeUPS, budget: prioEnum}
	fRange       = &family{name: "nut_variable_range_info", help: "Allowed range of a variable (LIST RANGE).", labels: []string{"variable", "min", "max"}, scope: scopeUPS, budget: prioRange}
	fBuildInfo   = &family{name: "nut_exporter_build_info", help: "Exporter build.", labels: []string{"version", "goversion"}, scope: -1}
)

var families = []*family{
	fServerUp, fServerInfo, fServerQuery, fServerSuppressed, fDuration,
	fUPSInfo, fUPSUp, fQuery, fQueryError, fMetaAge, fMetaErrors, fMetaPending, fSuppressed,
	fParseErrors, fLogins, fValue, fInfo, fStatus, fTimestamp, fClients, fCommands, fWritable,
	fType, fEnum, fRange, fBuildInfo,
}

// catFamilies are the catalogue-named gauge families (design §5b), by metric name.
var catFamilies = map[string]*family{}

func init() {
	fixed := map[string]bool{}
	for _, f := range families {
		fixed[f.name] = true
	}
	for _, c := range catalog.Families() {
		if fixed[c.Name] {
			panic("catalogue family " + c.Name + " collides with a fixed family")
		}
		f := &family{name: c.Name, help: c.Help(), scope: scopeUPS, budget: prioVariables}
		if c.Label != "" {
			f.labels = []string{c.Label}
		}
		catFamilies[c.Name] = f
		families = append(families, f)
	}
}

// gaugeSample returns the family and labels of a numeric variable: its catalogue family when the
// NUT catalogue names it, nut_variable_value otherwise.
func gaugeSample(sc []string, name string) (*family, []string) {
	if m, ok := catalog.Lookup(name); ok {
		f := catFamilies[m.Family.Name]
		if len(f.labels) > 0 {
			return f, with(sc, m.LabelValue)
		}
		return f, with(sc)
	}
	return fValue, with(sc, name)
}

func (f *family) labelNames() []string {
	switch f.scope {
	case scopeServer:
		return append([]string{"server"}, f.labels...)
	case scopeUPS:
		return append([]string{"server", "ups"}, f.labels...)
	default:
		return f.labels
	}
}

func (f *family) desc() *prometheus.Desc {
	return prometheus.NewDesc(f.name, f.help, f.labelNames(), nil)
}

func (f *family) valueType() prometheus.ValueType {
	if f.kind == 0 {
		return prometheus.GaugeValue
	}
	return f.kind
}
