# Copyright 2026 Benjamin Saiz
# SPDX-License-Identifier: Apache-2.0
"""Compare what each collector exposes for every variable of every dump (run.sh writes OUTDIR).

For each variable a verdict per tool: verbatim (exact text exposed), number (equal number), flags
(ups.status as one flag per word), converted (word mapped to a number or a bool), altered, or
missing. An exposition the Prometheus parser rejects (invalid.tsv) counts as missing whole.
"""
import json
import os
import re
import sys
from collections import Counter, defaultdict

OUT = sys.argv[1]
idx = json.load(open(os.path.join(OUT, "index.json")))
lookup = json.load(open(os.path.join(OUT, "lookup.json")))
invalid = {line.split("\t")[0] for line in open(os.path.join(OUT, "invalid.tsv")) if line.strip()}
NUM = re.compile(r"^-?[0-9]+(\.[0-9]+)?$")  # NUT numeric format: classes of values
DECIMAL = re.compile(r"^[-+]?([0-9]+\.?[0-9]*|\.[0-9]+)([eE][-+]?[0-9]+)?$")  # any decimal: scoring
IDENT = re.compile(r"(^|\.)(vendorid|productid|serial|id|firmware(\.aux)?|version(\.[a-z]+)?|date|mfr\.date|macaddr|part|model|mfr)$")


def number(raw):  # the same rule for every tool: any decimal spelling, spaces around allowed
    t = raw.strip(" ")
    return float(t) if DECIMAL.match(t) else None


def close(a, b):
    return a is not None and b is not None and abs(a - b) <= 1e-9 * max(1.0, abs(b))


def lossless(raw):  # the number gives back the exact text (no leading or trailing zero lost)
    v = float(raw)
    return str(int(v)) == raw if "." not in raw else repr(v) == raw


def value_class(name, raw):
    t = raw.strip(" ")
    if raw == "":
        return "empty"
    if NUM.match(t):
        if IDENT.search(name):
            return "number-like identifier"
        return "padded number" if t != raw else "number"
    if raw in ("enabled", "disabled"):
        return "enabled/disabled"
    if '"' in raw or "\\" in raw:
        return "text with \" or \\"
    if raw != t:
        return "padded text"
    return "text"


# ---- Prometheus text format ----
SERIES = re.compile(r'^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{(.*)\})? (\S+)')


def labels(s):
    out, i = {}, 0
    while i < len(s):
        while s[i] in ", ":
            i += 1
        k = s.index("=", i)
        key, i, v = s[i:k].strip(), k + 2, ""
        while s[i] != '"':
            if s[i] == "\\":
                # unknown escapes stay as written, as in the Prometheus parser
                v, i = v + {"n": "\n", "\\": "\\", '"': '"'}.get(s[i + 1], s[i:i + 2]), i + 2
            else:
                v, i = v + s[i], i + 1
        out[key], i = v, i + 1
        while i < len(s) and s[i] in ", ":
            i += 1
    return out


def samples(path):
    """Samples of a valid exposition; none for one the Prometheus parser rejected."""
    if path in invalid or not os.path.exists(path):
        return []
    out = []
    for line in open(path, encoding="utf-8", errors="replace"):
        if line.strip() and not line.startswith("#"):
            m = SERIES.match(line.rstrip("\n"))
            out.append((m.group(1), labels(m.group(3) or ""), float(m.group(4))))
    return out


def by_ups(path, label):
    out = defaultdict(list)
    for n, l, v in samples(path):
        if label in l:
            out[l[label]].append((n, l, v))
    return out


# ---- Telegraf ----
def unescape_split(s, sep):
    out, cur, i = [], "", 0
    while i < len(s):
        if s[i] == "\\" and i + 1 < len(s):
            cur, i = cur + s[i + 1], i + 2
            continue
        if s[i] == sep:
            out.append(cur)
            cur = ""
        else:
            cur += s[i]
        i += 1
    return out + [cur]


def parse_line(line):
    line = line[2:] if line.startswith("> ") else line
    i = 0
    while i < len(line) and line[i] != " ":
        i += 2 if line[i] == "\\" else 1
    head, rest = line[:i], line[i + 1:line.rfind(" ")]
    tags = dict(p.split("=", 1) for p in unescape_split(head, ",")[1:])
    fields, j = {}, 0
    while j < len(rest):
        k = rest.index("=", j)
        key, j = rest[j:k], k + 1
        if rest[j] == '"':
            j, v = j + 1, ""
            while rest[j] != '"':
                if rest[j] == "\\" and rest[j + 1] in '"\\':
                    v, j = v + rest[j + 1], j + 2
                else:
                    v, j = v + rest[j], j + 1
            fields[key], j = ("string", v), j + 1
        else:
            e = rest.find(",", j)
            e = len(rest) if e < 0 else e
            raw, j = rest[j:e], e
            if raw in ("true", "false"):
                fields[key] = ("bool", raw == "true")
            elif raw[-1] in "iu":
                fields[key] = ("int", float(raw[:-1]))
            else:
                fields[key] = ("float", float(raw))
        if j < len(rest) and rest[j] == ",":
            j += 1
    return tags, fields


def load_telegraf(name):
    out = {}
    for line in open(os.path.join(OUT, name)):
        if line.startswith("> upsd,"):
            tags, fields = parse_line(line.rstrip("\n"))
            out[tags["ups_name"]] = fields
    return out


# telegraf renames these (plugins/inputs/upsd/upsd.go defaultFieldSet); others: dots -> underscores
TELEGRAF_NAMES = {
    "battery.charge": "battery_charge_percent", "battery.runtime.low": "battery_runtime_low",
    "battery.voltage": "battery_voltage", "input.frequency": "input_frequency",
    "input.transfer.high": "input_transfer_high", "input.transfer.low": "input_transfer_low",
    "input.voltage": "input_voltage", "ups.temperature": "internal_temp", "ups.load": "load_percent",
    "battery.voltage.nominal": "nominal_battery_voltage", "input.voltage.nominal": "nominal_input_voltage",
    "ups.realpower.nominal": "nominal_power", "output.voltage": "output_voltage",
    "ups.realpower": "real_power", "ups.delay.shutdown": "ups_delay_shutdown",
    "ups.delay.start": "ups_delay_start"}


def telegraf_key(name):
    return TELEGRAF_NAMES.get(name, name.replace(".", "_"))


def verdict_telegraf(fields, name, raw):
    f = fields.get(telegraf_key(name))
    if f is None:
        return "missing", None
    typ, v = f
    if typ == "string":
        return ("verbatim" if v == raw else "altered"), typ
    if typ == "bool" or number(raw) is None:
        return "converted", typ
    return ("number" if close(v, number(raw)) else "altered"), typ


def telegraf_prom_index(series):
    metrics, labs = {}, {}
    for n, l, v in series:
        metrics[n] = v
        labs.update(l)
    return metrics, labs


def verdict_telegraf_prom(ix, name, raw):
    metrics, labs = ix
    key = re.sub(r"[^a-zA-Z0-9_]", "_", telegraf_key(name))  # Prometheus name rules
    if "upsd_" + key in metrics:
        v = metrics["upsd_" + key]
        if number(raw) is None:
            return "converted", "gauge"  # bool fields become 1/0
        return ("number" if close(v, number(raw)) else "altered"), "gauge"
    # String fields become labels of every series of the family (string_as_label); a label a UPS
    # does not set is empty, which Prometheus treats as absent. Bool fields are not exported.
    if labs.get(key, "") != "" or (key in labs and raw == ""):
        return ("verbatim" if labs[key] == raw else "altered"), "label"
    return "missing", None


# ---- DRuggeri nut_exporter ----
DEVICE_LABELS = {"model", "mfr", "serial", "type", "description", "contact", "location", "part", "macaddr"}


def druggeri_index(ups):
    gauges, device, flags = {}, {}, set()
    for n, l, v in samples(os.path.join(OUT, "druggeri", ups + ".prom")):
        if n == "network_ups_tools_device_info":
            device = l
        elif n == "network_ups_tools_ups_status":
            if v == 1:
                flags.add(l["flag"])
        else:
            gauges[n] = v
    return gauges, device, flags


def verdict_druggeri(ix, name, raw):
    gauges, device, flags = ix
    if name == "ups.status" and flags:
        return ("flags" if flags == set(raw.split()) else "altered"), "flags"
    p = name.split(".")
    if p[0] == "device" and len(p) == 2 and p[1] in DEVICE_LABELS and device.get(p[1], "") != "":
        return ("verbatim" if device[p[1]] == raw else "altered"), "label"
    g = gauges.get("network_ups_tools_" + name.replace(".", "_").replace("-", "_"))
    if g is None:
        return "missing", None
    if number(raw) is None:
        return "converted", "gauge"  # on/off regex mapped to 1/0
    return ("number" if close(g, number(raw)) else "altered"), "gauge"


# ---- HON95 prometheus-nut-exporter (tables read from its v1.2.1 source) ----
src = open(os.path.join(OUT, "hon95-src", "src", "metrics.rs")).read()
HON_METRICS = {}  # nut var -> (metric, transform)
for m in re.finditer(r'metric: "([^"]+)".*?nut_var: "([^"]*)",\s*var_transform: VarTransform::(\w+)', src, re.S):
    if m.group(2) and m.group(3) != "OldUpsStatus" and m.group(2) not in HON_METRICS:
        HON_METRICS[m.group(2)] = (m.group(1), m.group(3))
HON_LABELS = defaultdict(list)  # nut var -> nut_ups_info labels
for m in re.finditer(r'add_var_label\("([^"]+)", "([^"]+)"\)',
                     open(os.path.join(OUT, "hon95-src", "src", "openmetrics_builder.rs")).read()):
    HON_LABELS[m.group(2)].append(m.group(1))


def hon95_ok(ups):
    return open(os.path.join(OUT, "hon95", ups + ".code")).read().strip() == "200"


def hon95_index(ups):
    gauges, info, flags = {}, {}, set()
    for n, l, v in samples(os.path.join(OUT, "hon95", ups + ".prom")) if hon95_ok(ups) else []:
        if n == "nut_ups_info":
            info = l
        elif n == "nut_ups_status":
            if v == 1:
                flags.add(l["status"])
        else:
            gauges[n] = v
    return gauges, info, flags


def verdict_hon95(ix, name, raw):
    gauges, info, flags = ix
    if name == "ups.status":
        if not gauges and not info:
            return "missing", None
        return ("flags" if flags == set(raw.split()) else "altered"), "flags"
    for lab in HON_LABELS.get(name, []):
        if lab in info:
            return ("verbatim" if info[lab] == raw else "altered"), "label"
    metric, transform = HON_METRICS.get(name, (None, None))
    if metric is None or metric not in gauges:
        return "missing", None
    v = gauges[metric]
    if transform == "BeeperStatus":  # enabled/disabled/muted as 1/2/3, anything else 0
        return ("converted" if raw in ("enabled", "disabled", "muted") else "altered"), "gauge"
    if number(raw) is None:
        return "altered", "gauge"
    n = number(raw) / 100 if transform == "Percentage" else number(raw)  # percent as a 0-1 ratio
    return ("number" if close(v, n) else "altered"), "gauge"


# ---- dominikh/go-nut nut_exporter (table read from its source) ----
GONUT = {}  # nut var -> (metric, is_enum)
for m in re.finditer(r'"([a-z][a-z0-9.]*)":\s*\{name: "([^"]+)"[^}]*?(enum: "status")?\}',
                     open(os.path.join(OUT, "go-nut", "nutcollector", "collector.go")).read()):
    GONUT[m.group(1)] = ("nut_" + m.group(2), bool(m.group(3)))
gonut = by_ups(os.path.join(OUT, "go-nut.prom"), "name")


def verdict_gonut(series, name, raw):
    if not series:
        return "missing", None
    if name in ("device.model", "device.mfr", "device.serial", "device.type"):
        return ("verbatim" if series[0][1][name.split(".")[1]] == raw else "altered"), "label"
    metric, enum = GONUT.get(name, (None, False))
    for n, l, v in series:
        if n == metric:
            if enum:
                return ("verbatim" if l.get("status") == raw else "altered"), "label"
            return ("number" if close(v, number(raw)) else "altered"), "gauge"
    return "missing", None


# ---- jzck upsc-exporter (rules of its upsc_to_prometheus.awk) ----
def upsc_index(ups):
    metrics, labs = {}, defaultdict(dict)
    for n, l, v in samples(os.path.join(OUT, "upsc", ups + ".prom")):
        metrics[n] = v
        labs[n[len("upsc_"):].split("_")[0]].update(l)  # labels hang on series of the same prefix
    return metrics, labs


def verdict_upsc(ix, name, raw):
    metrics, labs = ix
    key = re.sub(r"^outlet_([0-9]+)", r"outlet\1", name.replace(".", "_"))
    if "upsc_" + key in metrics:
        if number(raw) is None:  # ups.status: exactly OL -> 0, OB -> 1
            return ("converted" if raw in ("OL", "OB") else "altered"), "gauge"
        return ("number" if close(metrics["upsc_" + key], number(raw)) else "altered"), "gauge"
    prefix = key.split("_")[0]
    label = key[len(prefix) + 1:]
    if label in labs.get(prefix, {}):
        return ("verbatim" if labs[prefix][label] == raw else "altered"), "label"
    return "missing", None


# ---- this project ----
ours = defaultdict(dict)
for n, l, v in samples(os.path.join(OUT, "ours.prom")):
    if l.pop("server", None) == "ddl" and "ups" in l:
        ours[l.pop("ups")][(n, tuple(sorted(l.items())))] = v
inventory = {u["name"]: {v["name"]: v for v in u["variables"]}
             for s in json.load(open(os.path.join(OUT, "ours.inventory.json")))["servers"] for u in s["ups"]}


def verdict_ours(ups, name, raw):
    v = inventory[ups].get(name)
    if v is None or v.get("suppressed"):
        return "missing", None
    series = ours[ups]
    if v["export"] == "info":
        key = ("nut_variable_info", tuple(sorted({"variable": name, "value": raw}.items())))
        return ("verbatim" if key in series else "altered"), "nut_variable_info"
    lab = {}
    if v["metric"] == "nut_variable_value":
        lab["variable"] = name
    elif name in lookup and lookup[name][0] == v["metric"] and lookup[name][1]:
        lab[lookup[name][1]] = lookup[name][2]
    got = series.get((v["metric"], tuple(sorted(lab.items()))))
    return ("number" if close(got, number(raw)) else "altered"), v["metric"]


# ---- scoring ----
TOOLS = ["telegraf (line protocol)", "telegraf force_float", "telegraf (prometheus)",
         "DRuggeri nut_exporter", "HON95", "go-nut", "upsc-exporter", "this project"]
tel = {"telegraf (line protocol)": load_telegraf("telegraf-false.out"),
       "telegraf force_float": load_telegraf("telegraf-true.out")}
tel_prom = by_ups(os.path.join(OUT, "telegraf-prom.prom"), "ups_name")
rows = []  # tool, name, raw, verdict, form
empty = 0
for d in idx:
    u = d["ups"]
    ix = {"telegraf (prometheus)": telegraf_prom_index(tel_prom.get(u, [])),
          "DRuggeri nut_exporter": druggeri_index(u), "HON95": hon95_index(u), "upsc-exporter": upsc_index(u)}
    for var in d["vars"]:
        name, raw = var["Name"], var["Value"]
        if raw == "":  # in Prometheus an empty label value is the same as no label: not comparable
            empty += 1
            continue
        for t in TOOLS:
            if t in tel:
                vd = verdict_telegraf(tel[t].get(u, {}), name, raw)
            elif t == "telegraf (prometheus)":
                vd = verdict_telegraf_prom(ix[t], name, raw)
            elif t == "DRuggeri nut_exporter":
                vd = verdict_druggeri(ix[t], name, raw)
            elif t == "HON95":
                vd = verdict_hon95(ix[t], name, raw)
            elif t == "go-nut":
                vd = verdict_gonut(gonut.get(u, []), name, raw)
            elif t == "upsc-exporter":
                vd = verdict_upsc(ix[t], name, raw)
            else:
                vd = verdict_ours(u, name, raw)
            rows.append((t, name, raw) + vd)

total = sum(len(d["vars"]) for d in idx) - empty
print(f"{len(idx)} dumps, {total} variables compared ({empty} empty values left out)")
bad = Counter()
for p in invalid:
    rel = os.path.relpath(p, OUT)
    bad[rel.split("/")[0] if "/" in rel else rel] += 1
print("expositions rejected by the Prometheus parser:", dict(bad) or "none")
print("HON95 scrapes not answered 200:", sum(1 for d in idx if not hon95_ok(d["ups"])), "of", len(idx))
failed = os.path.join(OUT, "upsc", "upsc-failed")
print("upsc runs that failed:", sum(1 for _ in open(failed)) if os.path.exists(failed) else 0, "of", len(idx))


def kept(cls, c, equivalent=False):
    n = c["verbatim"] + (c["lossless"] if cls == "number-like identifier" else c["number"])
    return n + (c["flags"] + c["converted"] if equivalent else 0)


def pct(n, d):  # one decimal, two when it would round to 100 without being 100
    x = 100 * n / d
    return f"{x:.2f}%" if 99.95 <= x < 100 else f"{x:.1f}%"


by, size = defaultdict(Counter), Counter()
for t, name, raw, vd, form in rows:
    cls = value_class(name, raw)
    by[(t, cls)][vd] += 1
    if vd == "number" and cls == "number-like identifier" and lossless(raw):
        by[(t, cls)]["lossless"] += 1
    if t == TOOLS[0]:
        size[cls] += 1
W = 26
print("\nShare of values exposed unchanged (text verbatim, number equal, identifier without loss)")
print(f"{'class':24}{'count':>7}" + "".join(f"{t:>{W}}" for t in TOOLS))
for cls in sorted(size, key=lambda c: -size[c]):
    print(f"{cls:24}{size[cls]:7}" + "".join(f"{pct(kept(cls, by[(t, cls)]), size[cls]):>{W}}" for t in TOOLS))
print(f"{'all':24}{total:7}" + "".join(f"{pct(sum(kept(c, by[(t, c)]) for c in size), total):>{W}}" for t in TOOLS))
print(f"{'all, equivalent forms':24}{total:7}" + "".join(
    f"{pct(sum(kept(c, by[(t, c)], True) for c in size), total):>{W}}" for t in TOOLS))
print("  (equivalent forms: ups.status as flags, words as bool or numbers, counted too)")

print("\nVerdicts (share of all variables)")
cats = ["verbatim", "number", "flags", "converted", "altered", "missing"]
print(f"{'tool':26}" + "".join(f"{c:>11}" for c in cats))
for t in TOOLS:
    c = Counter(r[3] for r in rows if r[0] == t)
    print(f"{t:26}" + "".join(f"{100 * c[k] / total:10.1f}%" for k in cats))

print("\nNUT names exported by >= 2 dumps in more than one form (field type, or metric family)")
for t in TOOLS:
    forms = defaultdict(Counter)
    for tt, name, raw, vd, form in rows:
        if tt == t and form:  # absent is not a form
            forms[name][form] += 1
    seen = [n for n, c in forms.items() if sum(c.values()) >= 2]
    print(f"{t:26}{sum(1 for n in seen if len(forms[n]) > 1):4} of {len(seen)}")

print("\nUPS series exposed (Prometheus outputs)")
count = {
    "telegraf (prometheus)": sum(len(s) for s in tel_prom.values()),
    "DRuggeri nut_exporter": sum(sum(1 for n, _, _ in samples(os.path.join(OUT, "druggeri", d["ups"] + ".prom"))
                                     if n.startswith("network_ups_tools_")) for d in idx),
    "HON95": sum(sum(1 for _, l, _ in samples(os.path.join(OUT, "hon95", d["ups"] + ".prom")) if "ups" in l)
                 for d in idx if hon95_ok(d["ups"])),
    "go-nut": sum(len(s) for s in gonut.values()),
    "upsc-exporter": sum(len(samples(os.path.join(OUT, "upsc", d["ups"] + ".prom"))) for d in idx),
    "this project": sum(len(s) for s in ours.values()),
}
for t, n in count.items():
    print(f"{t:26}{n:8}")

muted = [d["ups"] for d in idx for v in d["vars"] if v["Name"] == "ups.beeper.status" and v["Value"] == "muted"]
print(f"\nups.beeper.status=muted ({len(muted)} dumps):")
for t in TOOLS:
    print(f"  {t:26}{dict(Counter(r[3] for r in rows if r[0] == t and r[1] == 'ups.beeper.status' and r[2] == 'muted'))}")
