# Changelog

Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
[Semantic Versioning](https://semver.org/). Before 1.0.0, a minor version may change metric
names or configuration.

## [Unreleased]

### Changed

- `-nut.server` labels its server `default`, or the new `-nut.name`, instead of the address: a
  new address, or a move to a configuration file with the same `name`, keeps the series. Rename
  the `server` label in queries that used the address.

## [0.1.0] - 2026-11-09

First release.

### Added

- Collection of every variable of every UPS of one or more upsd servers in one scrape, read-only
  (no `SET`, `INSTCMD` or `LOGIN`, no NUT credentials), with per-server and per-UPS failure
  isolation, connection and command timeouts, and response size limits.
- Metrics named after the NUT catalogue (`docs/nut-names.txt` at NUT `1c4ab78`, 357 families) with
  the unit NUT states: `nut_battery_charge_percent`, `nut_input_voltage_volts{phase}`,
  `nut_outlet_current_amperes{outlet}`...; other numeric variables in `nut_variable_value`.
- Text variables in `nut_variable_info{variable,value}`, value verbatim; dates also as
  `nut_variable_timestamp_seconds`; hardware identity in `nut_device_info`; `ups.status` flags
  in `nut_ups_status{flag}`.
- Logins, client sessions, instant commands, writable variables and their type, enum and range
  (metadata refreshed in the background).
- Health and guard metrics: `nut_server_up`, `nut_ups_up`, `nut_query_success`,
  `nut_query_error_info`, `nut_suppressed`, `nut_server_suppressed`.
- `/inventory`: JSON snapshot of every raw value, query state and export decision.
- Guards against cardinality: label length, value churn, UPSes per server, series per UPS.
- Configuration: `-nut.server` or a YAML file (filters, classification overrides, guards,
  timeouts); listens on `:9199`.
- Container image `ghcr.io/disaster/nut-exporter` (linux/amd64, linux/arm64), distroless,
  non-root, with SBOM, build provenance and the licence files of every compiled-in module.

### Known limitations

- Filters and classification overrides apply to every server and UPS (per-UPS series can be
  dropped with Prometheus `metric_relabel_configs`).
- Values pass through unchanged: a driver reporting a wrong unit is not corrected.
- Validated against the NUT device dump library (725 dumps) and upsd 2.7.4; not yet run in
  production.

[Unreleased]: https://github.com/DisasteR/nut-exporter/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/DisasteR/nut-exporter/releases/tag/v0.1.0
