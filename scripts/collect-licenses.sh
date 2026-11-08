#!/bin/sh
# Copyright 2026 The nut-exporter Authors
# SPDX-License-Identifier: Apache-2.0
#
# collect-licenses.sh <package> <dest>: copies the licence and notice files of everything compiled
# into <package> to <dest>/<module>@<version>/, including files below a module root that cover
# vendored code (client_golang internal/github.com/golang/gddo). Fails when a module has none.
set -eu
pkg=$1 dest=$2
go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}}@{{.Version}} {{.Dir}}{{end}}{{end}} {{.Dir}}' "$pkg" |
	while read -r mod root dir; do
		[ -n "$dir" ] || continue
		# Package directory up to its module root.
		d=$dir
		while :; do
			for f in "$d"/LICENSE* "$d"/NOTICE* "$d"/COPYING*; do
				[ -f "$f" ] || continue
				rel=${f#"$root"/}
				mkdir -p "$dest/$mod/$(dirname "$rel")"
				[ -e "$dest/$mod/$rel" ] || cp "$f" "$dest/$mod/$rel" # several packages share a module
			done
			[ "$d" = "$root" ] && break
			d=$(dirname "$d")
		done
	done
for mod in $(go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}}@{{.Version}}{{end}}{{end}}' "$pkg" | sort -u); do
	ls "$dest/$mod" >/dev/null 2>&1 || { echo "no licence file for $mod" >&2; exit 1; }
done
