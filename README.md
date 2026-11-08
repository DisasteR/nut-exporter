# nut-exporter

Prometheus exporter for Network UPS Tools (upsd). It exports every variable of every UPS of one
or more upsd servers, without listing variables and without altering values, plus logins,
client addresses, instant commands, writable variables and their constraints. Read-only: it
sends no SET, INSTCMD or LOGIN and needs no NUT credentials. Design: `docs/design.md`.

## Run

```sh
nut-exporter -nut.server 127.0.0.1:3493
nut-exporter -config nut-exporter.yml
```

| Endpoint | Content |
|---|---|
| `/metrics` | Prometheus metrics, collected at scrape time |
| `/inventory` | JSON snapshot: every raw value, metadata, query state and export decision |

Set the scraper timeout above `timeouts.scrape` (10 s by default). `/inventory` holds serials and
descriptions: keep the listener private (`127.0.0.1:9199` by default).

## Metrics

Every UPS-scoped series has `server` (configured name, or the address with `-nut.server`) and
`ups` labels.

| Metric | Content |
|---|---|
| `nut_variable_value{variable}` | numeric variables, NUT units |
| `nut_variable_info{variable,value}` | text variables, value verbatim (`0002`, trailing spaces) |
| `nut_variable_timestamp_seconds{variable}` | `*.date` variables in `YYYY-MM-DD` or `YYYY/MM/DD` |
| `nut_ups_status{flag}` | `ups.status` flags, 0 or 1, only when `ups.status` was read |
| `nut_ups_logins`, `nut_ups_client_sessions{address}` | sessions logged in (upsmon) |
| `nut_ups_command_info{command}` | supported instant commands |
| `nut_variable_writable`, `nut_variable_type_info`, `nut_variable_enum_info`, `nut_variable_range_info` | variable metadata |
| `nut_server_up`, `nut_ups_up`, `nut_query_success{query}`, `nut_query_error_info{query,error}` | collection health |
| `nut_suppressed{family,reason}`, `nut_server_suppressed{reason}` | series not exported by a filter or guard |

A variable is a gauge when its name is not a documented opaque field (serials, IDs, dates,
firmware, versions, statuses...) and its value is a NUT number; otherwise it is text. Override
with `classify.numeric` / `classify.text`.

Examples:

```promql
nut_ups_status{flag="OB"} == 1                          # on battery
nut_ups_logins == 0                                     # no upsmon watches the UPS
time() - nut_variable_timestamp_seconds{variable="battery.mfr.date"} > 3 * 365 * 86400
nut_variable_value{variable="battery.charge"} * on(server, ups) group_left(value)
  nut_variable_info{variable="device.model"}
```

## Configuration

All keys are optional except `servers`; defaults shown.

```yaml
listen: 127.0.0.1:9199
servers:
  - name: nas
    address: 127.0.0.1:3493
ups:        { include: ["*"], exclude: [] }
variables:  { include: ["*"], exclude: [] }
classify:   { numeric: [], text: [] }
guard:      { max_length: 1024, max_distinct: 100, churn_window: 24h, max_ups: 256, max_series: 20000 }
metadata_ttl: 10m
timeouts:   { connect: 2s, command: 2s, scrape: 10s }
limits:     { max_line: 8192, max_response: 4194304 }
workers: 2
```

## Build and test

```sh
go test -race ./...
docker build --build-arg VERSION=<version> -t nut-exporter:<version> .
```
