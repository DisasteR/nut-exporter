// Copyright 2026 Benjamin Saiz
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Catches: the -nut.server label derived from the address again (a new address or a move to a
// configuration file would rename every series), -nut.name ignored, and flags silently dropped
// when combined with -config.
func TestLoadConfigServerName(t *testing.T) {
	file := filepath.Join(t.TempDir(), "c.yml")
	if err := os.WriteFile(file, []byte("servers: [{name: nas, address: 192.0.2.1:3493}]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		file, server, name string
		want               string // server name; "" when an error is expected
	}{
		{"", "192.0.2.1:3493", "", "default"},
		{"", "nas.example:3493", "", "default"},
		{"", "192.0.2.1:3493", "nas", "nas"},
		{file, "", "", "nas"},
		{file, "192.0.2.1:3493", "", ""},
		{file, "", "nas", ""},
		{"", "", "nas", ""},
		{"", "", "", ""},
	} {
		c, err := loadConfig(tt.file, tt.server, tt.name)
		if tt.want == "" {
			if err == nil {
				t.Errorf("%+v: accepted: %+v", tt, c.Servers)
			}
			continue
		}
		if err != nil {
			t.Errorf("%+v: %v", tt, err)
			continue
		}
		if len(c.Servers) != 1 || c.Servers[0].Name != tt.want {
			t.Errorf("%+v: servers %+v, want name %q", tt, c.Servers, tt.want)
		}
		if tt.file == "" && c.Servers[0].Address != tt.server {
			t.Errorf("%+v: address %q", tt, c.Servers[0].Address)
		}
	}
}
