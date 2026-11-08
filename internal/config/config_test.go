// Copyright 2026 Benjamin Saiz
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, content string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "c.yml")
	if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestLoadKeepsDefaultsForMissingKeys(t *testing.T) {
	c, err := Load(write(t, "servers: [{name: nas, address: 127.0.0.1:3493}]\ntimeouts: {scrape: 5s}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Timeouts.Scrape != 5*time.Second || c.Timeouts.Command != 2*time.Second || c.Workers != 2 || c.Guard.MaxDistinct != 100 {
		t.Fatalf("got %+v", c)
	}
}

func TestLoadRejects(t *testing.T) {
	tests := map[string]string{
		"unknown key":     "servers: [{name: a, address: x:1}]\nworkerz: 3\n",
		"duplicate name":  "servers: [{name: a, address: x:1}, {name: a, address: y:1}]\n",
		"empty name":      "servers: [{address: x:1}]\n",
		"empty address":   "servers: [{name: a}]\n",
		"no server":       "workers: 1\n",
		"bad pattern":     "servers: [{name: a, address: x:1}]\nvariables: {include: ['[']}\n",
		"zero workers":    "servers: [{name: a, address: x:1}]\nworkers: 0\n",
		"negative length": "servers: [{name: a, address: x:1}]\nguard: {max_length: -1}\n",
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(write(t, content)); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestFilterExcludeWins(t *testing.T) {
	f := Filter{Include: []string{"battery.*", "ups.status"}, Exclude: []string{"battery.date*"}}
	cases := map[string]bool{
		"battery.charge":     true,
		"battery.date":       false,
		"battery.date.maint": false,
		"ups.status":         true,
		"ups.load":           false,
	}
	for name, want := range cases {
		if got := f.Match(name); got != want {
			t.Errorf("Match(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestValidateReportsEveryError(t *testing.T) {
	c := Default()
	c.Workers = 0
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "no server") || !strings.Contains(err.Error(), "workers") {
		t.Fatalf("got %v", err)
	}
}
