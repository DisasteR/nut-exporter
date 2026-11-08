// Package collect reads every UPS of the configured upsd servers into snapshots (design §3).
package collect

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"time"

	"github.com/DisasteR/nut-exporter/internal/nut"
)

// Query states.
const (
	StateOK      = "ok"
	StateError   = "error"
	StatePending = "pending"
)

// Core per-UPS queries, in scheduling order.
const (
	QueryVars     = "vars"
	QueryLogins   = "logins"
	QueryClients  = "clients"
	QueryCommands = "commands"
	QueryRW       = "rw"
)

// CoreQueries lists the per-UPS queries run at every collection.
var CoreQueries = []string{QueryVars, QueryLogins, QueryClients, QueryCommands, QueryRW}

// Query is the outcome of one protocol query.
type Query struct {
	State  string    `json:"state"`
	At     time.Time `json:"at"`
	Error  string    `json:"error,omitempty"`  // known NUT error word, other, timeout, too_large, protocol or io
	Detail string    `json:"detail,omitempty"` // raw error text, for the inventory
}

// OK reports whether the query succeeded.
func (q Query) OK() bool { return q.State == StateOK }

// MetaEntry is one cached metadata answer. Value keeps the last successful answer, even after
// a failed refresh.
type MetaEntry struct {
	Query
	LastOK time.Time `json:"last_ok"`
	Value  []string  `json:"value"`
}

// VarMeta is the metadata of one variable. Range values are "min max" pairs, flattened.
type VarMeta struct {
	Type  MetaEntry `json:"type"`
	Desc  MetaEntry `json:"desc"`
	Enum  MetaEntry `json:"enum"`
	Range MetaEntry `json:"range"`
}

// UPS is the snapshot of one UPS.
type UPS struct {
	Name        string               `json:"name"`
	Description string               `json:"description"`
	Collected   bool                 `json:"collected"`
	SkipReason  string               `json:"skip_reason,omitempty"` // filter or ups_cap
	Queries     map[string]Query     `json:"queries,omitempty"`
	Vars        []nut.Var            `json:"-"`
	Logins      int                  `json:"logins"`
	Clients     []string             `json:"clients"`
	Commands    []string             `json:"commands"`
	RW          []nut.Var            `json:"-"`
	VarMeta     map[string]VarMeta   `json:"-"`
	CmdMeta     map[string]MetaEntry `json:"-"`
}

// Server is the snapshot of one upsd server.
type Server struct {
	Name     string           `json:"name"`
	Address  string           `json:"address"`
	Queries  map[string]Query `json:"queries"` // discovery, version, protocol
	Version  string           `json:"version"`
	Protocol string           `json:"protocol"`
	UPS      []UPS            `json:"ups"`
	Duration time.Duration    `json:"duration_ns"`
}

// Server query names.
const (
	QueryDiscovery = "discovery"
	QueryVersion   = "version"
	QueryProtocol  = "protocol"
)

// nutErrors are the error words of docs/net-protocol.txt; any other word maps to "other".
var nutErrors = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`ACCESS-DENIED UNKNOWN-UPS VAR-NOT-SUPPORTED CMD-NOT-SUPPORTED
		INVALID-ARGUMENT INSTCMD-FAILED SET-FAILED READONLY TOO-LONG FEATURE-NOT-SUPPORTED
		FEATURE-NOT-CONFIGURED ALREADY-SSL-MODE DRIVER-NOT-CONNECTED DATA-STALE ALREADY-LOGGED-IN
		INVALID-PASSWORD ALREADY-SET-PASSWORD INVALID-USERNAME ALREADY-SET-USERNAME
		USERNAME-REQUIRED PASSWORD-REQUIRED UNKNOWN-COMMAND INVALID-VALUE`) {
		nutErrors[w] = true
	}
}

// errorWord maps a client error to the bounded vocabulary of design §5.
func errorWord(err error) string {
	var nerr *nut.Error
	var neterr net.Error
	switch {
	case errors.As(err, &nerr):
		if nutErrors[nerr.Word] {
			return nerr.Word
		}
		return "other"
	case errors.Is(err, nut.ErrTooLarge):
		return "too_large"
	case errors.Is(err, nut.ErrProtocol):
		return "protocol"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded),
		errors.As(err, &neterr) && neterr.Timeout():
		return "timeout"
	default:
		return "io"
	}
}

func result(err error, at time.Time) Query {
	if err == nil {
		return Query{State: StateOK, At: at}
	}
	return Query{State: StateError, At: at, Error: errorWord(err), Detail: err.Error()}
}
