// Copyright 2026 The nut-exporter Authors
// SPDX-License-Identifier: Apache-2.0

// Package config loads and validates the exporter configuration (design §7).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path"
	"time"

	"gopkg.in/yaml.v3"
)

// Filter selects names with shell-style wildcards; Exclude wins over Include.
type Filter struct {
	Include []string `yaml:"include"`
	Exclude []string `yaml:"exclude"`
}

// Match reports whether name passes the filter.
func (f Filter) Match(name string) bool {
	return matchAny(f.Include, name) && !matchAny(f.Exclude, name)
}

// Server is one upsd endpoint.
type Server struct {
	Name    string `yaml:"name"`
	Address string `yaml:"address"`
}

// Classify holds the classification overrides.
type Classify struct {
	Numeric []string `yaml:"numeric"`
	Text    []string `yaml:"text"`
}

// Guard holds the projection guards (design §6).
type Guard struct {
	MaxLength   int           `yaml:"max_length"`
	MaxDistinct int           `yaml:"max_distinct"`
	ChurnWindow time.Duration `yaml:"churn_window"`
	MaxUPS      int           `yaml:"max_ups"`
	MaxSeries   int           `yaml:"max_series"`
}

// Timeouts bound network work.
type Timeouts struct {
	Connect time.Duration `yaml:"connect"`
	Command time.Duration `yaml:"command"`
	Scrape  time.Duration `yaml:"scrape"`
}

// Limits bound protocol answers, in bytes.
type Limits struct {
	MaxLine     int `yaml:"max_line"`
	MaxResponse int `yaml:"max_response"`
}

// Config is the whole configuration.
type Config struct {
	Listen      string        `yaml:"listen"`
	Servers     []Server      `yaml:"servers"`
	UPS         Filter        `yaml:"ups"`
	Variables   Filter        `yaml:"variables"`
	Classify    Classify      `yaml:"classify"`
	Guard       Guard         `yaml:"guard"`
	MetadataTTL time.Duration `yaml:"metadata_ttl"`
	Timeouts    Timeouts      `yaml:"timeouts"`
	Limits      Limits        `yaml:"limits"`
	Workers     int           `yaml:"workers"`
}

// Default returns the configuration used when no file is given.
func Default() Config {
	return Config{
		Listen:      "127.0.0.1:9199",
		UPS:         Filter{Include: []string{"*"}},
		Variables:   Filter{Include: []string{"*"}},
		Guard:       Guard{MaxLength: 1024, MaxDistinct: 100, ChurnWindow: 24 * time.Hour, MaxUPS: 256, MaxSeries: 20000},
		MetadataTTL: 10 * time.Minute,
		Timeouts:    Timeouts{Connect: 2 * time.Second, Command: 2 * time.Second, Scrape: 10 * time.Second},
		Limits:      Limits{MaxLine: 8192, MaxResponse: 4 << 20},
		Workers:     2,
	}
}

// Load reads a YAML file over the defaults. Unknown keys are errors.
func Load(file string) (Config, error) {
	c := Default()
	b, err := os.ReadFile(file)
	if err != nil {
		return c, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("%s: %w", file, err)
	}
	return c, c.Validate()
}

// Validate checks names, patterns and bounds.
func (c Config) Validate() error {
	var errs []error
	if len(c.Servers) == 0 {
		errs = append(errs, errors.New("no server configured"))
	}
	seen := map[string]bool{}
	for i, s := range c.Servers {
		switch {
		case s.Name == "":
			errs = append(errs, fmt.Errorf("servers[%d]: empty name", i))
		case seen[s.Name]:
			errs = append(errs, fmt.Errorf("servers[%d]: duplicate name %q", i, s.Name))
		}
		seen[s.Name] = true
		if s.Address == "" {
			errs = append(errs, fmt.Errorf("servers[%d]: empty address", i))
		}
	}
	for _, p := range concat(c.UPS.Include, c.UPS.Exclude, c.Variables.Include, c.Variables.Exclude, c.Classify.Numeric, c.Classify.Text) {
		if _, err := path.Match(p, ""); err != nil {
			errs = append(errs, fmt.Errorf("pattern %q: %w", p, err))
		}
	}
	positive := map[string]int64{
		"guard.max_length": int64(c.Guard.MaxLength), "guard.max_distinct": int64(c.Guard.MaxDistinct),
		"guard.churn_window": int64(c.Guard.ChurnWindow), "guard.max_ups": int64(c.Guard.MaxUPS),
		"guard.max_series": int64(c.Guard.MaxSeries), "metadata_ttl": int64(c.MetadataTTL),
		"timeouts.connect": int64(c.Timeouts.Connect), "timeouts.command": int64(c.Timeouts.Command),
		"timeouts.scrape": int64(c.Timeouts.Scrape), "limits.max_line": int64(c.Limits.MaxLine),
		"limits.max_response": int64(c.Limits.MaxResponse), "workers": int64(c.Workers),
	}
	for k, v := range positive {
		if v <= 0 {
			errs = append(errs, fmt.Errorf("%s must be positive", k))
		}
	}
	return errors.Join(errs...)
}

func matchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

func concat(lists ...[]string) []string {
	var out []string
	for _, l := range lists {
		out = append(out, l...)
	}
	return out
}
