#!/bin/sh
# Copyright 2026 Benjamin Saiz
# SPDX-License-Identifier: Apache-2.0
#
# Replays the NUT device dump library through one fake upsd and several NUT collectors (released
# images pinned by digest, or a pinned commit), then compares what each one exposes
# (docs/comparison.md).
# Usage: scripts/compare/run.sh OUTDIR   (needs go, docker, curl, python3, git; Linux host network)
set -eu

OUT=${1:?usage: run.sh OUTDIR}
NUT_DDL_REF=8b936185d4340b59bee09f4600780661d1e89c69
TELEGRAF=telegraf@sha256:695045186c69152ff71827ec0c4f40216fb10491597af71c00195d98be8c39f2                     # 1.40.3
DRUGGERI=druggeri/nut_exporter@sha256:276460d141c732772f8b1fcd785a7de272473e6d39fd85c32b163efc2999cb9a         # 3.3.0
HON95=hon95/prometheus-nut-exporter@sha256:a9bfbbae3b12a6486e6e4b933a69a79d01342db27346d6b4cd5512f4eef6b111    # 1.2.1
UPSC=jzck/upsc-exporter@sha256:7f8f8e957538e7286dede5858a1f5040ae9018b503ac03e733df3cc238149c29                # 2019-07-14
GONUT_REF=6a91d6710b741a03cd0cdf8852fb9940d0ad2a55                                                            # dominikh/go-nut
HON95_REF=36ff936c0e3b58aa7251050783a42431cc70edb5 # v1.2.1 source: the analysis reads its variable table
OURS=ghcr.io/disaster/nut-exporter@sha256:bd2ad5c444e33b3637cf5270995df257c6d3b9d20a667340eb36c76b13a107ec      # 0.3.0
OURS_TAG=v0.3.0 # the analysis maps names to series with this tree's catalogue: it must be the image's
UPSD=127.0.0.1:34930 # every dump as one UPS of this upsd
BASE=46000           # and dump i alone on port BASE+i
ID=cmp-$$
# the Accept header of a Prometheus scrape: each exporter answers in the format Prometheus would get
ACCEPT='application/openmetrics-text;version=1.0.0;q=0.5,application/openmetrics-text;version=0.0.1;q=0.4,text/plain;version=0.0.4;q=0.3,*/*;q=0.2'

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
cleanup() {
	docker rm -f "$ID-dr" "$ID-hon" "$ID-tp" "$ID-ours" >/dev/null 2>&1 || true
	[ -z "${GONUT:-}" ] || kill "$GONUT" 2>/dev/null || true
	[ -z "${FAKE:-}" ] || kill "$FAKE" 2>/dev/null || true
}
trap cleanup EXIT INT TERM
exec 3>&2 # die reports here, also from a command whose output wait_for discards
die() { echo "run.sh: $*" >&3; exit 1; }
# wait_for SECONDS COMMAND...: retries COMMAND every 0.5 s until it succeeds or SECONDS pass.
wait_for() {
	end=$(($(date +%s) + $1))
	shift
	until "$@" >/dev/null 2>&1; do
		if [ "$(date +%s)" -ge "$end" ]; then
			for c in $(docker ps -aq --filter "name=$ID"); do
				docker ps -a --filter "id=$c" --format '{{.Names}} {{.Status}}' >&2
				docker logs --tail 5 "$c" >&2 2>&1 || true
			done
			die "timed out: $*"
		fi
		sleep 0.5
	done
}
# fetch DIR URL COMMIT: shallow checkout of one commit, verified.
fetch() {
	[ -e "$1" ] || { git init -q "$1" && git -C "$1" fetch -q --depth 1 "$2" "$3" && git -C "$1" checkout -q FETCH_HEAD; }
	[ "$(git -C "$1" rev-parse HEAD)" = "$3" ] || die "$1 is not at $3"
	[ -z "$(git -C "$1" status --porcelain)" ] || die "$1 has local changes"
}

git -C "$REPO" diff --quiet "$OURS_TAG" -- internal/catalog/ ||
	die "internal/catalog differs from $OURS_TAG: the series lookup would not match the image"
fetch "$OUT/nut-ddl" https://github.com/networkupstools/nut-ddl "$NUT_DDL_REF"
fetch "$OUT/go-nut" https://github.com/dominikh/go-nut "$GONUT_REF"
fetch "$OUT/hon95-src" https://github.com/HON95/prometheus-nut-exporter "$HON95_REF"
rm -rf "$OUT/druggeri" "$OUT/hon95" "$OUT/upsc"
mkdir -p "$OUT/druggeri" "$OUT/hon95" "$OUT/upsc"

(cd "$REPO" && for t in fakesrv lookup; do go build -o "$OUT/$t" "./scripts/compare/$t"; done)
(cd "$HERE/scrapecheck" && go build -o "$OUT/scrapecheck" .)
: >"$OUT/scrapes.tsv" # path <TAB> content type of every Prometheus exposition
scrape() { # scrape FILE URL: GET with the Prometheus Accept header, records the content type
	ct=$(curl -fsS --max-time 600 -H "Accept: $ACCEPT" -o "$1" -w '%{content_type}' "$2")
	printf '%s\t%s\n' "$1" "$ct" >>"$OUT/scrapes.tsv"
}
(cd "$OUT/go-nut" && go build -o "$OUT/go-nut-exporter" ./cmd/nut_exporter)
"$OUT/fakesrv" -root "$OUT/nut-ddl" -listen "$UPSD" -base "$BASE" -index "$OUT/index.json" 2>"$OUT/fakesrv.log" &
FAKE=$!
fake_ready() { kill -0 "$FAKE" || die "fakesrv exited: $(cat "$OUT/fakesrv.log")"; grep -q ' ready: ' "$OUT/fakesrv.log"; }
wait_for 60 fake_ready
"$OUT/lookup" "$OUT/index.json" "$OUT/lookup.json"
N=$(python3 -c 'import json,sys; print(len(json.load(open(sys.argv[1]))))' "$OUT/index.json")
each() { # each COMMAND: runs COMMAND UPS PORT for every dump
	i=0
	while [ "$i" -lt "$N" ]; do
		"$@" "$(printf d%04d "$i")" $((BASE + i))
		i=$((i + 1))
	done
}

# Telegraf inputs.upsd: every field, IDs as strings. Native output (line protocol, --test) with and
# without force_float, then the Prometheus output of a running agent.
telegraf_conf() {
	cat <<EOF
[agent]
  omit_hostname = true
  interval = "10s"
  flush_interval = "1s"
[[inputs.upsd]]
  server = "127.0.0.1"
  port = ${UPSD##*:}
  additional_fields = ["*"]
  stringify_ids = true
  force_float = $1
EOF
}
for ff in false true; do
	{ telegraf_conf "$ff"; printf '[[outputs.file]]\n  files = ["stdout"]\n'; } >"$OUT/telegraf-$ff.conf"
	docker run --rm --network host -v "$OUT/telegraf-$ff.conf:/etc/telegraf/telegraf.conf:ro" \
		"$TELEGRAF" telegraf --test >"$OUT/telegraf-$ff.out" 2>"$OUT/telegraf-$ff.err"
done
{ telegraf_conf false; printf '[[outputs.prometheus_client]]\n  listen = "127.0.0.1:34935"\n  collectors_exclude = ["gocollector", "process"]\n'; } >"$OUT/telegraf-prom.conf"
docker run -d --name "$ID-tp" --network host -v "$OUT/telegraf-prom.conf:/etc/telegraf/telegraf.conf:ro" \
	"$TELEGRAF" >/dev/null
telegraf_ready() {
	n=$(curl -fsS http://127.0.0.1:34935/metrics | grep -c '^upsd_status_flags{' || true)
	echo "telegraf: $n of $N UPSes" >"$OUT/telegraf-prom.progress"
	[ "$n" -eq "$N" ]
}
wait_for 300 telegraf_ready
scrape "$OUT/telegraf-prom.prom" http://127.0.0.1:34935/metrics

# DRuggeri nut_exporter: every numeric variable (empty vars_enable), one scrape per UPS.
docker run -d --name "$ID-dr" --network host "$DRUGGERI" --nut.server=127.0.0.1 \
	--nut.serverport="${UPSD##*:}" --nut.vars_enable= --web.listen-address=127.0.0.1:34931 >/dev/null
wait_for 60 curl -fsS -o /dev/null http://127.0.0.1:34931/metrics
druggeri() { scrape "$OUT/druggeri/$1.prom" "http://127.0.0.1:34931/ups_metrics?ups=$1"; }
each druggeri

# HON95 prometheus-nut-exporter: probe each dump on its own port (one bad UPS fails its server).
docker run -d --name "$ID-hon" --network host -e HTTP_PORT=34936 "$HON95" >/dev/null
wait_for 60 curl -sS -o /dev/null http://127.0.0.1:34936/ # any HTTP answer
hon95() { # an HTTP error is a result here (one bad UPS fails its server): recorded, not fatal
	r=$(curl -sS --max-time 60 -H "Accept: $ACCEPT" -o "$OUT/hon95/$1.prom" -w '%{http_code}\t%{content_type}' \
		"http://127.0.0.1:34936/nut?target=127.0.0.1:$2")
	echo "${r%%	*}" >"$OUT/hon95/$1.code"
	[ "${r%%	*}" != 200 ] || printf '%s\t%s\n' "$OUT/hon95/$1.prom" "${r#*	}" >>"$OUT/scrapes.tsv"
}
each hon95

# dominikh/go-nut nut_exporter: every UPS of the upsd in one scrape.
"$OUT/go-nut-exporter" -h "$UPSD" -l 127.0.0.1:34937 2>"$OUT/go-nut.log" &
GONUT=$!
wait_for 60 curl -fsS -o /dev/null http://127.0.0.1:34937/metrics
scrape "$OUT/go-nut.prom" http://127.0.0.1:34937/metrics

# jzck upsc-exporter: NUT's upsc, piped into its awk script, for each dump.
docker run --rm --network host -e N="$N" -e BASE="$BASE" -v "$OUT/upsc:/out" --entrypoint sh "$UPSC" -c '
	cd / && i=0
	while [ "$i" -lt "$N" ]; do
		u=$(printf d%04d "$i")
		# an upsc failure is a result (recorded), not hidden by the awk step
		upsc "$u@127.0.0.1:$((BASE + i))" >/out/$u.upsc 2>/out/$u.err || echo "$u" >>/out/upsc-failed
		./upsc_to_prometheus.awk </out/$u.upsc >/out/$u.prom
		i=$((i + 1))
	done'
for f in "$OUT"/upsc/*.prom; do printf '%s\ttext/plain\n' "$f" >>"$OUT/scrapes.tsv"; done # its inetd wrapper's type

# This project: one server, every UPS in one scrape; guards raised above the corpus size (725 UPSes
# on one server, default max_ups 256). Scraped once the background metadata is complete.
cat >"$OUT/ours.yml" <<EOF
listen: 127.0.0.1:34932
servers: [{name: ddl, address: $UPSD}]
guard: {max_ups: 1000, max_series: 1000000}
timeouts: {scrape: 300s}
EOF
docker run -d --name "$ID-ours" --network host -v "$OUT/ours.yml:/etc/ours.yml:ro" "$OURS" \
	-config /etc/ours.yml >/dev/null
wait_for 60 curl -fsS -o /dev/null http://127.0.0.1:34932/metrics
pending() {
	curl -fsS http://127.0.0.1:34932/metrics |
		awk '/^nut_metadata_pending/ {n++; s += $NF} END {exit !(n > 0 && s == 0)}'
}
wait_for 3600 pending
scrape "$OUT/ours.prom" http://127.0.0.1:34932/metrics
curl -fsS -o "$OUT/ours.inventory.json" http://127.0.0.1:34932/inventory # static input: same values

# Expositions the Prometheus scrape parser rejects for their content type (Prometheus drops such a
# scrape whole).
"$OUT/scrapecheck" <"$OUT/scrapes.tsv" >"$OUT/invalid.tsv"

python3 -I "$HERE/analyze.py" "$OUT" >"$OUT/report.txt"
cat "$OUT/report.txt"
