# nut-exporter design

Prometheus exporter for Network UPS Tools (upsd). Goal: collect every piece of information upsd
publishes for every UPS of one or more NUT servers, without listing variables, without altering
values, with per-UPS and per-query failure isolation.

Facts are tagged [Certain] (read in a cited source), [Probable], [Supposition]. Source aliases:
`N` NUT master `1c4ab78`, `N@2.7.4` tag v2.7.4, `G` go.nut `60e196249c53`, `E` nut_exporter
`793425e`.

## 1. Requirements

- R1 Zero variable listing: every variable of every discovered UPS is collected and exported by
  default.
- R2 Multi-UPS, multi-server in one scrape; every UPS-scoped series carries `server` and `ups`.
- R3 Collection is complete and exact: the snapshot served on `/inventory` holds every decoded
  protocol value byte for byte, every query result and every query error.
- R4 Export is complete by default. Text is exported byte for byte (`0002`, `051d`, trailing
  spaces, `disabled`). A quantity is exported as a gauge: NUT defines `01200.2` and `1200.20` as
  representations of the same number (`N/docs/nut-names.txt:99-112`), so its text form is kept in
  the snapshot only.
- R5 The only exceptions to R4 are configured filters and resource guards (§6); each one is
  visible in metrics, with its scope and reason.
- R6 A failing UPS, query or metadata refresh never hides another UPS's data, nor the results of
  other queries of the same UPS.
- R7 Absence is never exported as 0.
- R8 Read-only on the NUT side; no credentials.

Non-goals: SET/INSTCMD/LOGIN/FSD; vendor knowledge (drivers already map hardware to NUT names);
derived states; history upsd does not keep (past self-test results); `/probe` and URL targets.

## 2. Protocol facts the design rests on

- [Certain] GET/LIST need no authentication in 2.7.4 nor master: no `FLAG_USER` on `GET`/`LIST`
  (`N/server/netcmds.h`, `N@2.7.4/server/netcmds.h:48-49`). upsd has no read authentication;
  access is controlled by `LISTEN` and the network. Hence no credentials, no STARTTLS in V1.
- [Certain] `GET TYPE` answers `NUMBER` for any variable the driver did not flag
  (`N@2.7.4/server/netget.c:159-162`, `N/server/netget.c:175-183`); on the real UPS every
  variable answers `NUMBER`. It still reports `ENUM`, `RANGE` and `STRING:<len>` for flagged
  variables (`N@2.7.4/server/netget.c:144-155`). Collected as metadata, never used to classify.
  2.7.4 drops `RW` on the NUMBER path (`:162`): writability comes from `LIST RW`.
- [Certain] Values are double-quoted, `\"` and `\\` escaped, parsed per `common/parseconf.c`
  (`N/docs/net-protocol.txt:62-85`). NUT's client parser drops bytes outside 0x20-0x7f
  (`parseconf.c`, `addchar`); this client keeps them.
- [Certain] `LIST VAR`, `GET NUMLOGINS`, `GET TYPE` fail with `ERR DATA-STALE` or
  `ERR DRIVER-NOT-CONNECTED` when the driver is stale or gone (`N/server/upsd.c:762-784`);
  `LIST UPS` and `LIST CLIENT` do not check driver state (`N@2.7.4/server/netlist.c:226-285`).
- [Certain] `GET NUMLOGINS` counts sessions that sent `LOGIN` (upsmon); `LIST CLIENT` lists their
  addresses, possibly repeated (`N/docs/net-protocol.txt:125-144,450-466`). Both since 2.6.4.
- [Certain] `ups.status` flags: OL OB LB HB RB CHRG DISCHRG BYPASS CAL OFF OVER TRIM BOOST FSD
  (`N/docs/new-drivers.txt:343-356`), ALARM added by NUT, WAIT set by upsd before the first driver
  dump (master doc; also observed on upsd 2.7.4 with dummy-ups: `ups.status="WAIT"` and 3
  variables for ~1 s after a driver restart, then a growing partial `LIST VAR`); other tokens
  are possible. Alerts on `OL` must tolerate `WAIT`.
- [Certain] `VER` and `NETVER` give server and protocol versions (`N@2.7.4/server/netmisc.c`);
  `server.info`/`server.version` are synthesized by `GET VAR` only, not in `LIST VAR`
  (`N@2.7.4/server/netget.c:165-192`, `netlist.c:121-127`). V1 uses `VER`/`NETVER`.
- [Certain] go.nut is lossy and fails as a whole: splits on `"` without unescaping and trims
  spaces (`G/ups.go:133-134`), turns `enabled`/`disabled` into bools and digit strings into
  numbers (`G/ups.go:150-175`), and `GetUPSList` returns at the first UPS whose metadata query
  fails (`G/nut.go:132-135`). Not used.

## 3. Architecture

```
HTTP /metrics ──► collection coordinator ──► per server: core collector (scrape time)
HTTP /inventory ─┘        (shared, own deadline)      └─► metadata refresher (background)
                                     both use the read-only NUT client, separate connections
```

Go, `prometheus/client_golang`, standard library otherwise.

NUT client (`internal/nut`): tokenizer per parseconf; `BEGIN LIST`/`END LIST` matched against the
request; immediate `ERR` handling; bounded line (8 KiB) and response (4 MiB); dial and per-command
deadlines; a connection that hit a timeout or protocol error is closed, never reused (its stream
may be out of sync). Commands implemented: `VER`, `NETVER`, `LIST UPS|VAR|CMD|RW|CLIENT|ENUM|RANGE`,
`GET NUMLOGINS|TYPE|DESC|CMDDESC`, `LOGOUT`. No other command exists in the code.

Core collection (every scrape, per server, `workers` connections, default 2):
1. `LIST UPS` (discovery). `VER` and `NETVER` are independent server queries, run on their own
   connection while the UPS work proceeds: a failure only empties the matching label of
   `nut_server_info` and sets `nut_server_query_success` to 0.
2. A work queue of (UPS, query) items for `LIST VAR`, `GET NUMLOGINS`, `LIST CLIENT`, `LIST CMD`,
   `LIST RW`, ordered round-robin across UPSes (every `LIST VAR` first). Each item has its own
   command timeout and its own result.
3. Deferral: a query that times out closes and replaces its connection, and moves the UPS's
   remaining items to the back of the queue: healthy UPSes go first, the deferred items are
   still attempted while time remains. [Probable] upsd answers LIST/GET from in-memory state
   (`sstate_getnode`, `N/server/sstate.c`), so a timeout points to a server or network fault more
   than to one slow UPS.
4. Results are committed per item as they complete. At the deadline, finished items are exported,
   unfinished ones are reported as `timeout`.

Deadline envelope: with `W` workers, command timeout `t` and `H` UPSes whose first query times
out, healthy UPSes start after about `ceil(H/W)*t` plus reconnections; defaults (W=2, t=2 s,
scrape 10 s) keep healthy UPSes collected with up to 6 such UPSes per server. Deferred items then
use the remaining time; whatever is unfinished at the deadline is reported `timeout`.

Metadata refresh (background goroutine per server, own connection, own timeouts; the entry due
for the longest goes first, never-refreshed ones first, ties rotating across UPSes): per variable
`GET TYPE`, `GET DESC`, `LIST ENUM` and `LIST RANGE` (both lists always: in 2.7.4 `GET TYPE`
returns early on ENUM and hides a range list, `N@2.7.4/server/netget.c:144-155`); per command
`GET CMDDESC`. Every `metadata_ttl` (default 10 min) and when new variables or commands appear.
Each (UPS, item, query) entry has a state `pending | ok | error`, its value, last success time
and last error. A successful answer, empty included, replaces the value; a failed one keeps the
previous value. An entry is committed only if its item is still listed in the latest successful
`LIST VAR`/`LIST CMD` (generation check); an item no longer listed drops its entries. Scrapes
read the cache, never wait for it; a new UPS exports its metadata families once refreshed.
A failed entry is retried after 1 min, doubling up to `metadata_ttl`; a timeout pauses all work of
that UPS for 1 min, so other UPSes progress. Retained values are bounded per UPS by
`limits.max_response` bytes: an answer that would exceed it fails its entry with `too_large`
and keeps the previous value. Pending entries are exported
(`nut_metadata_pending`). Initial refresh of a healthy UPS costs `4N + C` round trips (N
variables, C commands), interleaved with other UPSes.

Concurrent scrapes share one in-flight collection whose lifetime is bounded by its own
`timeouts.scrape` (default 10 s), independent of any caller; a caller that disconnects or times
out stops waiting without cancelling it. Set the scraper's timeout above `timeouts.scrape`.
HTTP responses have a write deadline of `timeouts.scrape` + 10 s, so a stalled reader cannot hold
a handler and its snapshot.

Requests per scrape for one UPS: 3 per server + 5 per UPS. nut_exporter sends `2N+5` per UPS
(`GET DESC` and `GET TYPE` per variable, `G/ups.go:122-148`), 97 for this UPS.

## 4. Classification

Each variable is exported as exactly one of: a gauge, or a text info series.

1. Configured overrides: `classify.numeric`, `classify.text` (wildcards).
2. Name matches the built-in opaque list → text. It encodes NUT's documented opaque, identifier,
   enum and date fields, not vendors:
   `*.serial *.id *.vendorid *.productid *.date *.date.* *.time *.firmware *.firmware.*
   *.version *.version.* *.mfr *.model *.name *.desc *.description *.contact *.location *.part
   *.macaddr *.type *.status *.alarm *.result *.reason *.sensitivity *.mode *.feed.color
   driver.parameter.* driver.flag.* driver.state`.
3. Value matches the NUT numeric grammar `^-?[0-9]+(\.[0-9]+)?$` (`nut-names.txt:97-124`) →
   gauge.
4. Anything else → text.

A deny list of opaque names rather than an allow list of numeric names: drivers publish many
numeric names outside the catalogue (220 numeric `experimental.*` entries in `N/drivers/*.c`);
as text each new value would be a new series and the churn guard would hide them. Residual risk:
an undocumented digit-only identifier becomes a gauge; `classify.text` fixes it, and its exact
text stays in `/inventory`.

A variable forced numeric whose value does not parse is exported as text for that scrape and
counted in `nut_variable_parse_errors_total`.

Dates: a text variable named `*.date` or `*.date.*` whose value is `YYYY-MM-DD` or `YYYY/MM/DD`
(one separator kind) and a valid calendar date (`time.Parse`) also yields
`nut_variable_timestamp_seconds` (00:00 UTC); invalid dates (`2026-99-99`, `2026/11-08`) yield
none.
Year-first is unambiguous; `11/07/26` yields none. [Certain] the APC driver writes
`%04d/%02d/%02d` (`N/drivers/apc-hid.c:136`, same in 2.7.4).

## 5. Metrics

Labels: `server` (configured name), `ups` (NUT name). Values in NUT units.

| Family | Extra labels | Source / value |
|---|---|---|
| `nut_server_up` | | 1 if `LIST UPS` succeeded |
| `nut_server_info` | `version`, `protocol` | 1; raw `VER`, `NETVER` (label empty if that query failed or its answer is not a valid label; the latter shows as an empty label with `nut_server_query_success` 1; raw in `/inventory`) |
| `nut_server_query_success` | `query` | 1/0 for `discovery`, `version`, `protocol` |
| `nut_server_suppressed` | `reason` | UPSes not exported: `filter`, `ups_cap` (not collected), `encoding` (name not UTF-8), `duplicate` |
| `nut_ups_info` | `description` | 1; `LIST UPS`; exported whenever the UPS is listed |
| `nut_ups_up` | | 1 if `LIST VAR` succeeded this scrape |
| `nut_query_success` | `query` | 1/0 per core query of this scrape (`vars`, `logins`, `clients`, `commands`, `rw`) |
| `nut_query_error_info` | `query`, `error` | 1 per failed query; `error` = one of the 23 NUT error words of net-protocol.txt, `other` for any other word, `timeout`, `too_large`, `protocol` or `io`; the raw text is in `/inventory` |
| `nut_<catalogue name>[_<unit>]` | index labels (§5b) | gauge; numeric variable named in the NUT catalogue |
| `nut_variable_value` | `variable` | gauge; numeric variable outside the NUT catalogue |
| `nut_variable_info` | `variable`, `value` | 1 |
| `nut_variable_timestamp_seconds` | `variable` | Unix time |
| `nut_ups_status` | `flag` | 0/1, only when `ups.status` was read |
| `nut_ups_logins` | | `GET NUMLOGINS` |
| `nut_ups_client_sessions` | `address` | sessions per address, `LIST CLIENT` |
| `nut_ups_command_info` | `command` | 1 per `LIST CMD` entry |
| `nut_variable_writable` | `variable` | 1 per `LIST RW` entry |
| `nut_variable_type_info` | `variable`, `type` | 1 per `GET TYPE` token (cache) |
| `nut_variable_enum_info` | `variable`, `value` | 1 per allowed value (cache) |
| `nut_variable_range_info` | `variable`, `min`, `max` | 1 per range (cache) |
| `nut_metadata_age_seconds` | `query` | age of the oldest successful entry per query kind |
| `nut_metadata_errors` | `query` | entries whose last refresh failed |
| `nut_metadata_pending` | `query` | entries never refreshed yet |
| `nut_suppressed` | `family`, `reason` | per UPS: series not exported this scrape (§6) |
| `nut_variable_parse_errors_total` | `variable` | counter |
| `nut_collect_duration_seconds` | | per server |
| `nut_exporter_build_info` | `version`, `goversion` | 1 |

Each family depends only on its own query: a failed `LIST VAR` removes variable-derived families
(`nut_variable_*` from vars, `nut_ups_status`) for that UPS and nothing else. Type, enum and
range families come from the metadata cache only and stay while `LIST VAR` fails.

Status: known flags (§2) are emitted once `ups.status` is read; an unknown flag is emitted as 1
and remembered so it later reads 0 (max 32 per UPS, the first ones seen, in name order within a
scrape; kept until the UPS disappears). Beyond 32,
further unknown flags are not projected (`family="nut_ups_status"`, `reason="status_tokens"`);
the raw `ups.status` stays in `nut_variable_info` and `/inventory`.

Descriptions (`GET DESC`, `GET CMDDESC`) go to `/inventory` only: long, static, localised text.

Identity: `device.mfr`/`model`/`serial` are ordinary `nut_variable_info` series; dashboards join
on `variable`. No duplicate identity metric.

## 5b. Catalogue-named metrics

NUT standardises variable names, meanings and units (`N/docs/nut-names.txt`). A numeric variable
named in that catalogue gets its own metric; any other numeric variable stays in
`nut_variable_value{variable}`, so export remains exhaustive. Each value is exported once.

Source: the variable tables and the three-phase SPEC tables of `nut-names.txt` at a pinned NUT
commit. A generator (`internal/catalog/gen`) writes `internal/catalog/catalog_gen.go`; the NUT
text itself is not vendored, only names, index patterns and unit words. Regenerating is a
deliberate change, reviewed through the generated diff (a renamed metric breaks dashboards).

Patterns:
- literal names (`battery.charge`);
- `ambient.n.*`, `outlet.n.*`, `outlet.group.n.*`: `n` is a decimal index, exported as label
  `ambient`, `outlet`, `outlet_group` (raw text, `01` and `1` stay distinct); every `outlet.n.X`
  also exists as `outlet.group.n.X` with the same unit (nut-names.txt: the outlet data apply to
  outlet groups);
- three-phase `DOMAIN.CONTEXT.SPEC` and `DOMAIN.SPEC`, DOMAIN in `input`, `input.bypass`,
  `input.servicebypass`, `output`, `output.bypass`, `output.inverter`, `output.servicebypass`,
  CONTEXT in `L1-L2 L2-L3 L3-L1 L1-N L2-N L3-N L1 L2 L3 N`, exported as label `phase` (`""` for
  `DOMAIN.SPEC`). Every domain takes every SPEC of the per-phase table (wider than the
  catalogue's examples); aggregate-only SPECs (`frequency`) never take a context:
  `input.L1.frequency` stays generic.
- Unindexed forms join their family with the label empty: `DOMAIN.SPEC`, and `ambient.X` /
  `outlet.X` for every `ambient.n.X` / `outlet.n.X` (NUT: `ambient.0` is equivalent to `ambient`;
  drivers publish `ambient.temperature`). `ambient=""` and `ambient="0"` both denote the default
  sensor, `outlet="0"` the whole unit, `phase=""` the aggregate: select labels before summing.
- Names defined in prose only: `input.phases`, `output.phases`. `outlet.group.count` (number of
  groups) is not `outlet.group.n.count` (outlets of a group): generic. A table literal joins an
  indexed family only as a documented equivalent, else the generator fails. `device.uptime`
  states its unit in the sentence ("in seconds"); `*.percent` names carry the unit.
  Placeholders `x`/`xxx` (`driver.parameter.xxx`) are text and skipped.
- Entries the catalogue marks opaque (description containing "opaque") are always text: the
  generator emits them as `catalog.Text` patterns, used by classification (§4) with the
  built-in opaque list.

Name: `nut_` + the pattern without its index/context part, `.` and `-` replaced by `_`, + a unit
suffix when the catalogue states a unit in the description's last parentheses and the name does
not already end with it: `V` volts, `A` amperes, `percent` / `percent of ...` percent, `seconds`
seconds, `degrees C` celsius, `degrees` degrees, `Hz` hertz, `VA` / `Volt-Amps` voltamperes,
`W` / `Watts` watts, `Ah` ampere_hours, `min` minutes. Final parentheses that are not units are
listed (`yes/no`, `dimensionless ...`); any other text fails the generation, so a new unit is
reviewed. A pattern stating no unit takes the one its base name states elsewhere (spec `load`
takes `percent` from `input.load`). No value is converted: the suffix states the NUT unit, and
NUT sentinels pass through (`ups.timer.shutdown` -1 = no timer running). Example: `battery.runtime` -> `nut_battery_runtime_seconds`,
`input.L1-N.voltage` -> `nut_input_voltage_volts{phase="L1-N"}`,
`ambient.1.temperature` -> `nut_ambient_temperature_celsius{ambient="1"}`.

Rules:
- Classification (§4) decides first: a catalogue variable whose value is not a gauge stays text in
  `nut_variable_info`. Only gauges take the catalogue name.
- HELP states the NUT name pattern and unit, no NUT prose; descriptions are in `/inventory`
  (`GET DESC`).
- The generator fails when two patterns give one metric name with different labels or units. A
  name equal to a fixed family (`ups.status`, `server.info`) or under `nut_variable_` is left to
  the generic families and listed in the generated `excluded`. The lookup is injective (a
  variable name and its family + label value determine each other), so two distinct variables
  never share a series.
- A unit is never inherited from a parent name: `battery.voltage.low` states none and has no
  suffix (upstream documentation gap, not guessed). A variable whose value turns non-numeric moves to `nut_variable_info`, as
  for generic gauges.
- Guards and budget: same as `nut_variable_value` (group = variable name, `prioVariables`);
  filters match the NUT variable name.
- `/inventory` gives each variable's exported metric name (`metric`). Selecting every numeric
  variable at once is no longer one family: use `/inventory` or `{__name__=~"nut_.*"}`.

## 6. Filters and guards

Filters and guards act on the Prometheus projection only, except the UPS filter and `max_ups`,
which skip collection of those UPSes. Defaults are sized so a normal installation never hits
them. Each firing is counted in `nut_suppressed{family,reason}` (per UPS, number of series not
exported) or `nut_server_suppressed{reason}` (number of UPSes), and the value stays in
`/inventory`.

| Reason | Rule | Default |
|---|---|---|
| `filter` | `ups`/`variables` include/exclude (exclude wins) | include all |
| `encoding` | text value not valid UTF-8 (Prometheus label values must be UTF-8) | always |
| `length` | label value longer than `max_length` bytes; never truncated or hashed | 1024 |
| `churn` | turnover: the label sets of a (family, variable) key seen within `churn_window` but absent now reach `max_distinct` (a single-valued variable is suppressed at its `max_distinct`-th distinct value); applies to `nut_variable_info`, clients, type, enum, range. The size of a family never triggers it: commands and writable names are static driver data, bounded by `series_cap` | 100 / 24 h |
| `duplicate` | a name repeated in one LIST answer (variable, RW) or in `LIST UPS`: the last one wins, the others are counted; duplicate series would make the registry fail the whole scrape. `LIST UPS` duplicates are resolved before the UPS filter and `max_ups` | always |
| `ups_cap` | more than `max_ups` per server; UPSes kept in name order | 256 |
| `series_cap` | more than `max_series` UPS-scoped series; health, query and suppression families are outside the budget, as is `nut_ups_logins`; families dropped whole in this order: enum, range, type, writable, commands, clients, timestamps; then variables in reverse name order, text and gauges alike | 20000 |
| `status_tokens` | unknown status flags beyond 32 | 32 |

Retained state is bounded: metadata and churn entries exist only for listed items, and listed
items are bounded by `limits.max_response` per LIST answer and `max_ups` per server (worst case
for a hostile server: `max_ups` x 5 core answers x `max_response` per collection, about 5 GiB with
defaults; lower `max_ups`/`max_response` where that matters); a churn key keeps at most `max_distinct + 1` value hashes, then only a
saturated mark until the window slides; unknown status flags at most 32 per UPS; metadata entries
only for listed items, their values at most `limits.max_response` bytes per UPS (each value
counted as its bytes + 16). State is keyed by server/UPS/item and dropped when the item is absent from
a successful `LIST UPS`/`LIST VAR`/`LIST CMD`. Churn state is in memory; a restart resets it.

## 6b. Inventory

`GET /inventory` returns JSON, schema version 1:

```json
{"schema": 1, "collected_at": "...", "servers": [{
  "name": "nas", "address": "127.0.0.1:3493",
  "queries": {"discovery": {"state": "ok", "at": "...", "error": ""}, "version": {...}, "protocol": {...}},
  "version": "...", "protocol": "...",
  "ups": [{"name": "ups", "description": "", "collected": true, "complete": false,
    "queries": {"vars": {"state": "ok", "at": "..."}, "clients": {"state": "error", "error": "timeout"}},
    "variables": [{"name": "device.serial", "value": "XX0000X00000  ",
                   "export": "info", "suppressed": "length",
                   "metadata": {"type": {"state": "ok", "value": ["NUMBER"]}, "desc": {...},
                                "enum": {...}, "range": {...}}}],
    "commands": [...], "rw": [...], "clients": [...], "logins": 1}]}]}
```

- Every protocol string (names, variable and RW values, descriptions, enum and range values,
  client addresses, command names, versions) is a JSON string when valid UTF-8, else an object
  `{"base64": "..."}`: reversible, also inside lists. `rw` entries are `{name, value}`.
- `complete` is true only when every core query and every metadata entry of the UPS is `ok`.
- When `LIST VAR` fails, `variables` lists the cached variables with `"value": null` and their
  metadata; when `LIST CMD` fails, `commands` lists the cached commands with their description.
- A UPS skipped by the UPS filter or `max_ups` is listed with `"collected": false` and the reason.
- Response limits (`limits.max_line` per line, newline included; `limits.max_response` per `LIST`
  answer, every byte including `BEGIN`/`END` lines; single-line answers are bounded by
  `max_line`) are configurable; exceeding them
  fails that query with `too_large`, and the snapshot reports it incomplete.

## 7. Configuration

Zero configuration: `--nut.server=127.0.0.1:3493` (server name = the address). Optional YAML:

```yaml
listen: 127.0.0.1:9199
servers:                       # names must be unique and non-empty: checked at startup
  - name: nas
    address: 127.0.0.1:3493
ups:        { include: ["*"], exclude: [] }
variables:  { include: ["*"], exclude: [] }
classify:   { numeric: [], text: [] }
guard:      { max_length: 1024, max_distinct: 100, churn_window: 24h, max_ups: 256, max_series: 20000 }
metadata_ttl: 10m
timeouts:   { connect: 2s, command: 2s, scrape: 10s }
limits:     { max_line: 8192, max_response: 4194304 }   # bytes
workers: 2
```

Wildcards are shell-style on the dotted name. Servers come only from configuration. `/inventory`
is served on the same listener as `/metrics` and contains serials and descriptions.

## 8. Failure semantics

| Situation | Exported for that UPS/server |
|---|---|
| Server unreachable or `LIST UPS` fails | `nut_server_up 0`; UPS series of that server absent |
| `VER`/`NETVER` fails | `nut_server_query_success{query="version"} 0`; everything else unaffected |
| UPS listed, `LIST VAR` fails (stale, no driver) | `nut_ups_info`, `nut_ups_up 0`, `nut_query_error_info{query="vars",error="DATA-STALE"}`, plus every other query that succeeded (clients, commands...) |
| `LIST CLIENT` fails, vars ok | everything but `nut_ups_client_sessions`; `nut_query_error_info{query="clients"}` |
| Metadata refresh entry fails | previous value of that entry kept, `nut_metadata_errors` > 0 |
| Shared deadline hit | finished queries exported, unfinished ones `error="timeout"` |

HTTP status stays 200 whenever the exporter works; NUT failures are metrics.

## 9. Tests

Fake upsd: in-process TCP server scripted per scenario, recording every request; per-command
delays and errors. Each test states which wrong implementation it catches.

- Fixture `testdata/apc-back-ups-rs-1500g.txt`: raw `LIST VAR` lines of the real UPS (43
  variables, trailing spaces kept, serial replaced by a same-length fake), with provenance. The
  expected exposition is a separate hand-written file, not generated by the code under test.
- `0002`, `051d`, `disabled`, trailing spaces, empty value, `\"`, `\\`, non-UTF-8 round-trip into
  `/inventory`; exports match §4/§6.
- A fake answering `NUMBER` to every `GET TYPE` changes no classification; `GET TYPE` is never sent
  by the core collector.
- Two UPSes with the same variables; two servers exposing a UPS of the same name; duplicate server
  names rejected at startup; a UPS added between scrapes appears.
- One UPS `ERR DATA-STALE` on `LIST VAR` keeps its clients/commands and the other UPS's data.
- Two UPSes hanging on every command and a healthy third UPS: the third is fully exported within
  the deadline; the hanging ones report `timeout`.
- Only `LIST VAR` of a UPS times out, its other queries answer at once: they are all exported.
- Metadata: one UPS's metadata requests time out repeatedly; a newly discovered healthy UPS still
  gets its metadata.
- `VER` failing: discovery and all UPS data still exported.
- Variables succeed, metadata hangs: variables exported, metadata absent or stale, scrape on time.
- Partial metadata refresh: one `LIST ENUM` fails, others succeed; enum values change; a
  variable with both an enum and a range list keeps both; ENUM→none clears the enum; a variable
  deleted while its refresh is in flight is not restored.
- Two overlapping scrapes, one caller disconnecting: the other gets a full result.
- Status: repeated spaces, duplicates, unknown token, token disappearing → 1 then 0; no status
  series when `LIST VAR` fails; 33 unknown tokens.
- Guards: each reason fires and is reported; a numeric-only UPS above `max_series` drops gauges
  in reverse name order; a client address churning past `max_distinct` is suppressed.
- Dates: valid, invalid day, Feb 29 on leap and non-leap years, mixed separators.
- Inventory: non-UTF-8 value round-trips through `value_base64`; `complete` false on any error.
- Hanging server, missing `END`, oversized line, `ERR` mid-list: bounded time, connection
  discarded.
- Request recorder fails the test on any command outside §3's list.
- Mutation checks that must fail the suite: numeric guessing on opaque names, no unescaping,
  aborting all UPSes on one error, zeros for unread status, metadata on the scrape path.
- `go test -race`. CI integration against real upsd with `dummy-ups`: Debian 11 (NUT 2.7.4) and a
  current NUT, fixture above plus a second UPS.

## 10. Packaging and deployment

- Multi-stage build, static binary, distroless `nonroot` runtime; base images pinned by digest;
  image tag = release version; changelog per tag.
- NAS: run next to `druggeri/nut_exporter:3.3.0` on another port; port the `nas-ups-energy`
  dashboard, alert rules and the `DISCHRG` annotation to the new names; then remove nut_exporter
  and its NUT credentials. Rollback: the old service stays in the compose file until then.

## 11. Open questions

- `ups.timer.shutdown=-1` meaning on usbhid-ups before alerting on it.
- Real scrape latency and series count on the NAS, to be measured on the bench.
- Repository name, owner and licence.
