package export

import (
	"encoding/base64"
	"encoding/json"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/DisasteR/nut-exporter/internal/classify"
	"github.com/DisasteR/nut-exporter/internal/collect"
)

// pstr is a protocol string: a JSON string when valid UTF-8, else {"base64": "..."}.
type pstr string

func (s pstr) MarshalJSON() ([]byte, error) {
	if utf8.ValidString(string(s)) {
		return json.Marshal(string(s))
	}
	return json.Marshal(map[string]string{"base64": base64.StdEncoding.EncodeToString([]byte(s))})
}

func pstrs(l []string) []pstr {
	out := make([]pstr, len(l))
	for i, s := range l {
		out[i] = pstr(s)
	}
	return out
}

type invQuery struct {
	State  string    `json:"state"`
	At     time.Time `json:"at,omitzero"`
	Error  string    `json:"error,omitempty"`
	Detail pstr      `json:"detail,omitempty"`
}

type invMeta struct {
	invQuery
	LastOK time.Time `json:"last_ok,omitzero"`
	Value  []pstr    `json:"value"`
}

type invVar struct {
	Name       pstr               `json:"name"`
	Value      *pstr              `json:"value"`                // null when LIST VAR failed (cached metadata only)
	Export     string             `json:"export,omitempty"`     // gauge or info, before guards
	Metric     string             `json:"metric,omitempty"`     // family the value is exported in, before guards
	Suppressed string             `json:"suppressed,omitempty"` // reason the series was not exported
	Metadata   map[string]invMeta `json:"metadata,omitempty"`
}

type invRW struct {
	Name  pstr `json:"name"`
	Value pstr `json:"value"`
}

type invCmd struct {
	Name        pstr     `json:"name"`
	Description *invMeta `json:"description,omitempty"`
}

type invUPS struct {
	Name        pstr                `json:"name"`
	Description pstr                `json:"description"`
	Collected   bool                `json:"collected"`
	SkipReason  string              `json:"skip_reason,omitempty"`
	Complete    bool                `json:"complete"`
	Queries     map[string]invQuery `json:"queries,omitempty"`
	Variables   []invVar            `json:"variables,omitempty"`
	RW          []invRW             `json:"rw,omitempty"`
	Commands    []invCmd            `json:"commands,omitempty"`
	Clients     []pstr              `json:"clients,omitempty"`
	Logins      *int                `json:"logins,omitempty"`
}

type invServer struct {
	Name     string              `json:"name"`
	Address  string              `json:"address"`
	Queries  map[string]invQuery `json:"queries"`
	Version  pstr                `json:"version"`
	Protocol pstr                `json:"protocol"`
	UPS      []invUPS            `json:"ups"`
}

type inventory struct {
	Schema      int         `json:"schema"`
	CollectedAt time.Time   `json:"collected_at"`
	Servers     []invServer `json:"servers"`
}

func iq(q collect.Query) invQuery {
	return invQuery{State: q.State, At: q.At, Error: q.Error, Detail: pstr(q.Detail)}
}

func im(e collect.MetaEntry) invMeta {
	return invMeta{invQuery: iq(e.Query), LastOK: e.LastOK, Value: pstrs(e.Value)}
}

func (e *Exporter) inventory(res *Result) inventory {
	inv := inventory{Schema: 1, CollectedAt: res.At}
	for _, srv := range res.Servers {
		is := invServer{Name: srv.Name, Address: srv.Address, Queries: map[string]invQuery{},
			Version: pstr(srv.Version), Protocol: pstr(srv.Protocol)}
		for q, st := range srv.Queries {
			is.Queries[q] = iq(st)
		}
		for _, u := range srv.UPS {
			is.UPS = append(is.UPS, e.invUPS(res, srv.Name, u))
		}
		inv.Servers = append(inv.Servers, is)
	}
	return inv
}

func (e *Exporter) invUPS(res *Result, server string, u collect.UPS) invUPS {
	iu := invUPS{Name: pstr(u.Name), Description: pstr(u.Description), Collected: u.Collected, SkipReason: u.SkipReason}
	if !u.Collected {
		return iu
	}
	complete := true
	iu.Queries = map[string]invQuery{}
	for q, st := range u.Queries {
		iu.Queries[q] = iq(st)
		complete = complete && st.OK()
	}
	meta := func(name string) map[string]invMeta {
		m, ok := u.VarMeta[name]
		if !ok {
			complete = false
			return nil
		}
		out := map[string]invMeta{
			collect.MetaType: im(m.Type), collect.MetaDesc: im(m.Desc),
			collect.MetaEnum: im(m.Enum), collect.MetaRange: im(m.Range),
		}
		for _, x := range out {
			complete = complete && x.State == collect.StateOK
		}
		return out
	}
	if u.Queries[collect.QueryVars].OK() {
		for _, v := range u.Vars {
			export, metric := "info", fInfo.name
			if k, _, _ := e.proj.cls.Classify(v.Name, v.Value); k == classify.Gauge {
				f, _ := gaugeSample([]string{server, u.Name}, v.Name)
				export, metric = "gauge", f.name
			}
			val := pstr(v.Value)
			iu.Variables = append(iu.Variables, invVar{Name: pstr(v.Name), Value: &val, Export: export, Metric: metric,
				Suppressed: res.varSup[key(server, u.Name, v.Name)], Metadata: meta(v.Name)})
		}
	} else {
		// LIST VAR failed: the cache still knows the variables and their metadata, not their values.
		for _, name := range sortedKeys(u.VarMeta) {
			iu.Variables = append(iu.Variables, invVar{Name: pstr(name), Metadata: meta(name)})
		}
	}
	for _, v := range u.RW {
		iu.RW = append(iu.RW, invRW{Name: pstr(v.Name), Value: pstr(v.Value)})
	}
	cmds := append([]string(nil), u.Commands...)
	if !u.Queries[collect.QueryCommands].OK() {
		cmds = sortedKeys(u.CmdMeta)
	}
	sort.Strings(cmds)
	for _, c := range cmds {
		ic := invCmd{Name: pstr(c)}
		if m, ok := u.CmdMeta[c]; ok {
			x := im(m)
			ic.Description = &x
			complete = complete && m.State == collect.StateOK
		} else {
			complete = false
		}
		iu.Commands = append(iu.Commands, ic)
	}
	iu.Clients = pstrs(u.Clients)
	if u.Queries[collect.QueryLogins].OK() {
		n := u.Logins
		iu.Logins = &n
	}
	iu.Complete = complete
	return iu
}
