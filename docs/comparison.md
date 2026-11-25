# Comparison with other NUT collectors

Measured on 2026-11-25 by `scripts/compare/run.sh` (images pinned by digest, sources by commit):

| Called below | Collector | Version | Activity |
|---|---|---|---|
| **Telegraf** | [influxdata/telegraf](https://github.com/influxdata/telegraf) `inputs.upsd` | 1.40.3 | released 2026-11-02 |
| **DRuggeri nut_exporter** | [DRuggeri/nut_exporter](https://github.com/DRuggeri/nut_exporter) | 3.3.0 | released 2026-05-15 |
| **HON95** | [HON95/prometheus-nut-exporter](https://github.com/HON95/prometheus-nut-exporter) | 1.2.1 | last release 2022-08-03 |
| **go-nut** | [dominikh/go-nut](https://github.com/dominikh/go-nut) `cmd/nut_exporter` | commit `6a91d67` | no release; last commit 2020-07-06 |
| **upsc-exporter** | [jzck/prometheus-upsc-exporter](https://github.com/jzck/prometheus-upsc-exporter) | image built 2019-07-14 | no release; last commit 2019-07-14 |
| **this project** | [DisasteR/nut-exporter](https://github.com/DisasteR/nut-exporter) | 0.3.0 | |

These results describe those versions and settings only; rerun the bench for others. Not
measured: twiedenbein/prometheus-nut-exporter (no licence file, no stars or forks).

## Method

`scripts/compare/run.sh OUTDIR` serves the 725 files of the NUT Devices Dumps Library
([nut-ddl](https://github.com/networkupstools/nut-ddl) at `8b93618`: 724 device dumps and one format
example) from a fake upsd that speaks the NUT network protocol, with replies modelled on upsd
2.7.4: as 725 UPSes of one server, and each dump alone on its own port. Each tool collects them
with these settings:

- Telegraf: `additional_fields = ["*"]`, `stringify_ids = true`; its native output (InfluxDB line
  protocol, with and without `force_float`) and `outputs.prometheus_client` with its defaults
  (`metric_version = 1`, `string_as_label = true`).
- DRuggeri nut_exporter: `--nut.vars_enable=` (every numeric variable), default on/off regular
  expressions, one scrape per UPS.
- HON95: one probe per dump.
- go-nut: one scrape of the server.
- upsc-exporter: NUT's `upsc` piped into its awk script, per dump.
- This project: one scrape of the server; `max_ups` (default 256, here 725 UPSes on one server),
  `max_series` and the scrape timeout raised; scraped once the background metadata is complete.

Prometheus endpoints are fetched with the `Accept` header of a Prometheus scrape and checked with
the Prometheus scrape parser (`model/textparse` of Prometheus 2.55.1) for the content type they
announce; a rejected exposition loses every value of that scrape.

`scripts/compare/analyze.py` then checks, for each of the 25321 non-empty values, whether it can be
read back, following each tool's own naming rules (HON95 and go-nut tables are read from their
source). A value counts as unchanged when text is exposed byte for byte, when a number equals the
NUT value as a float64 (a percentage exposed as a 0-1 ratio counts as equal), and, for an
identifier made of digits (`0002`, a version, a serial), when the number gives the exact text
back. `ups.status` as one flag per word, and words turned into numbers or booleans, are counted
apart ("equivalent forms"). The 193 empty values are left out: in Prometheus an empty label value
is the same as no label.

The fake upsd replays fixed values: changes over time, metadata (it answers `NUMBER` to every
`GET TYPE`, without enumerations or ranges), stale or disconnected drivers, latency and load on
upsd are not measured.

## Results

Share of values exposed unchanged, by kind of value:

| Kind of value | Count | Telegraf (InfluxDB) | Telegraf (Prometheus) | DRuggeri nut_exporter | HON95 | go-nut | upsc-exporter | This project |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| Number | 13232 | 100 % | 100 % | 99.98 % | 67.6 % | 64.3 % | 95.5 % | 100 % |
| Text | 9403 | 100 % | 100 % | 13.5 % | 41.6 % | 21.2 % | 76.2 % | 100 % |
| Identifier made of digits | 1792 | 61.3 % | 61.3 % | 61.3 % | 67.2 % | 2.5 % | 61.2 % | 100 % |
| `enabled` / `disabled` | 467 | 0 % | 0 % | 0 % | 0 % | 81.6 % | 89.5 % | 100 % |
| Text with leading or trailing spaces | 239 | 0 % | 0 % | 0 % | 30.1 % | 37.7 % | 52.7 % | 100 % |
| Number padded with spaces | 180 | 100 % | 100 % | 100 % | 0 % | 0 % | 80.6 % | 100 % |
| Text containing `"` or `\` | 8 | 0 % | 0 % | 0 % | 0 % | 25 % | 25 % | 100 % |
| **All** | **25321** | **94.4 %** | **94.4 %** | **62.3 %** | **55.8 %** | **43.5 %** | **85.3 %** | **100 %** |
| All, equivalent forms counted | 25321 | 96.3 % | 94.4 % | 67.7 % | 60.0 % | 43.5 % | 87.3 % | 100 % |

Numbers are compared as float64: four very large driver values (a 255-digit load, battery
temperatures near 3e18) lose digits in every tool that exposes them as numbers, this one included.

What each tool does with the values:

- **Telegraf** and **DRuggeri nut_exporter** read upsd through the
  [robbiet480/go.nut](https://github.com/robbiet480/go.nut) library, which turns digit strings into
  numbers (`0002` -> 2), `enabled`/`disabled` into booleans, trims spaces and cuts a value at its
  first `"`. With the tested defaults, Telegraf's Prometheus output does not export boolean fields
  and puts every string field as a label on every series of a metric (140 labels per series in the
  median, up to 396, empty where a UPS has no such field); `metric_version = 2` exports booleans
  as 1/0, and `string_as_label = false` drops the text. DRuggeri nut_exporter has no text but its
  `device_info` labels: 29.2 % of the values do not appear.
- **HON95** and **go-nut** export a fixed list of variables (56 and 71 NUT names, labels included)
  and drop the others; neither reads a number padded with spaces. HON95 exposes percentages as
  0-1 ratios and the beeper state as 1/2/3. One value containing `"` makes HON95 answer HTTP 503 for
  its target (2 of the 725 dumps, each on its own port here); from its source, every UPS of a
  shared server would be lost.
- **upsc-exporter** reads through NUT's own client, then turns names and text into Prometheus
  names and labels without checking them: 17 of the 725 expositions are rejected by the Prometheus
  parser (14 metric names with a `-`, such as three-phase `L1-N`; one label name starting with a
  digit; two values with an unescaped `"` or `\`), losing every value of those UPSes.

Same NUT variable exported in more than one form (field type, or metric family) depending on the
device, among the names exported for at least two dumps: Telegraf 157 of 619 in line protocol (39
with `force_float`; an InfluxDB shard accepts one type per field), 38 of 613 in Prometheus;
upsc-exporter 80 of 570; this project 10 of 619 (driver values outside the NUT numeric format on
some devices); 0 for the others, which skip what does not fit their list.

UPS series in one Prometheus scrape of these 725 UPSes: Telegraf 16039, DRuggeri nut_exporter
27419, HON95 20420, go-nut 9637, upsc-exporter 14646, this project 52435: 37935 for the variables
(values, 11537 status flags, identity, date timestamps) and 14500 per-UPS information and
collection health; no `nut_variable_type_info`, since the fake upsd answers `NUMBER` to every
`GET TYPE` (a real upsd adds one series per `RW`, `ENUM`, `RANGE` or `STRING:<len>` token). These
are counts of one snapshot, not of label churn or storage cost.

## Where the others fit better

- An existing Telegraf and InfluxDB pipeline: Telegraf also derives `time_left_ns` and the
  apcupsd-compatible `status_flags`.
- Fewer series: every other tool exports less, mostly by dropping text and metadata.
- Percentages as 0-1 ratios (HON95), the Prometheus base-unit convention; this project keeps the
  NUT unit (`nut_battery_charge_percent`).

## Trade-offs

- Words as numbers. DRuggeri nut_exporter maps on/off words to 1/0 (its regular expressions are
  configurable), Telegraf turns `enabled`/`disabled` into booleans and HON95 codes the beeper state
  as 1/2/3, so an alert can compare a number. A word outside the expected ones is lost or changes
  type: `ups.beeper.status: muted` (13 dumps) is not exported by DRuggeri nut_exporter, and is a
  string in Telegraf where other devices give a boolean. This project keeps the word and alerts on
  it: `nut_variable_info{variable="ups.beeper.status", value!="enabled"} == 1` (a UPS that does not
  publish the variable has no series, so the rule does not fire for it).

## Not measured here

From the source code, not from this bench: the go.nut library sends `GET DESC` and `GET TYPE` for
every variable on every collection, where this project refreshes metadata in the background with a
cache. Telegraf reads every UPS of a server through one go.nut call that fails as a whole when one
UPS fails, as HON95 does for its target; DRuggeri nut_exporter and upsc-exporter collect one UPS per
scrape; the go-nut exporter and this project report a failing UPS and keep the others.
