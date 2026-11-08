// Copyright 2026 The nut-exporter Authors
// SPDX-License-Identifier: Apache-2.0

// Package classify maps NUT values to gauges or opaque text.
package classify

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/DisasteR/nut-exporter/internal/catalog"
)

type Kind int

const (
	Gauge Kind = iota
	Text
)

// Opaque lists the built-in opaque name patterns.
var Opaque = []string{
	"*.serial", "*.id", "*.vendorid", "*.productid", "*.date", "*.date.*", "*.time",
	"*.firmware", "*.firmware.*", "*.version", "*.version.*", "*.mfr", "*.model", "*.model.*", "*.name",
	"*.desc", "*.description", "*.contact", "*.location", "*.part", "*.macaddr", "*.type",
	"*.status", "*.alarm", "*.result", "*.reason", "*.sensitivity", "*.mode", "*.feed.color",
	"driver.parameter.*", "driver.flag.*", "driver.state",
}

// Classifier holds validated overrides.
type Classifier struct{ numeric, text []string }

// New validates and copies override patterns.
func New(numeric, text []string) (*Classifier, error) {
	for _, patterns := range [][]string{numeric, text} {
		for _, pattern := range patterns {
			if _, err := path.Match(pattern, ""); err != nil {
				return nil, fmt.Errorf("pattern %q: %w", pattern, err)
			}
		}
	}
	return &Classifier{append([]string(nil), numeric...), append([]string(nil), text...)}, nil
}

func matches(patterns []string, name string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

var number = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)

// Classify gives text overrides precedence over numeric overrides.
func (c *Classifier) Classify(name, value string) (Kind, float64, bool) {
	if matches(c.text, name) {
		return Text, 0, false
	}
	forced := matches(c.numeric, name)
	if !forced && (matches(Opaque, name) || matches(catalog.Text, name)) {
		return Text, 0, false
	}
	// Fixed-width drivers (bcmxcp, upscode2, tripplite_usb) pad numbers with spaces; the value
	// itself is kept verbatim for text and the inventory.
	if n := strings.Trim(value, " "); number.MatchString(n) {
		if v, err := strconv.ParseFloat(n, 64); err == nil {
			return Gauge, v, false
		}
	}
	return Text, 0, forced
}

// Timestamp parses a calendar date at midnight UTC.
func Timestamp(name, value string) (int64, bool) {
	if !matches([]string{"*.date", "*.date.*"}, name) || len(value) != 10 {
		return 0, false
	}
	layout := "2006-01-02"
	if value[4] == '/' {
		layout = "2006/01/02"
	}
	t, err := time.Parse(layout, value)
	if err != nil {
		return 0, false
	}
	return t.Unix(), true
}
