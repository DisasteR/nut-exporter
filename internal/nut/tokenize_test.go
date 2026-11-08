// Copyright 2026 The nut-exporter Authors
// SPDX-License-Identifier: Apache-2.0

package nut

import (
	"errors"
	"reflect"
	"testing"
)

func TestTokenize(t *testing.T) {
	tests := []struct {
		name string
		line string
		want []string
	}{
		{"var line", `VAR ups battery.charge "100"`, []string{"VAR", "ups", "battery.charge", "100"}},
		{"leading zeros kept", `VAR ups ups.productid "0002"`, []string{"VAR", "ups", "ups.productid", "0002"}},
		{"trailing spaces kept", `VAR ups device.serial "XX0000X00000  "`, []string{"VAR", "ups", "device.serial", "XX0000X00000  "}},
		{"inner spaces kept", `VAR ups ups.firmware "878.L5 .I"`, []string{"VAR", "ups", "ups.firmware", "878.L5 .I"}},
		{"empty quoted", `UPS ups ""`, []string{"UPS", "ups", ""}},
		{"escaped quote", `VAR ups ups.alarm "say \"hi\""`, []string{"VAR", "ups", "ups.alarm", `say "hi"`}},
		{"escaped backslash", `VAR ups x "a\\b"`, []string{"VAR", "ups", "x", `a\b`}},
		{"escaped hash", `VAR ups x "Outlet \#3"`, []string{"VAR", "ups", "x", "Outlet #3"}},
		{"literal hash in quotes", `VAR ups x "Outlet #3"`, []string{"VAR", "ups", "x", "Outlet #3"}},
		{"quote closes element", `"one"two`, []string{"one", "two"}},
		{"quote inside word is literal", `ab"c`, []string{`ab"c`}},
		{"escaped space joins", `one\ two`, []string{"one two"}},
		{"backslash n is letter n", `a\nb`, []string{"anb"}},
		{"equals splits", `a=b`, []string{"a", "=", "b"}},
		{"comment", `a b # c d`, []string{"a", "b"}},
		{"comment right after word", `ab#c`, []string{"ab"}},
		{"multiple spaces and tabs", "a  \t b", []string{"a", "b"}},
		{"non-ascii kept", "VAR ups x \"\xe9t\xe9\"", []string{"VAR", "ups", "x", "\xe9t\xe9"}},
		{"empty line", ``, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Tokenize(tt.line)
			if err != nil {
				t.Fatalf("Tokenize(%q) error: %v", tt.line, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Tokenize(%q) = %q, want %q", tt.line, got, tt.want)
			}
		})
	}
}

func TestTokenizeUnterminatedQuote(t *testing.T) {
	if _, err := Tokenize(`VAR ups x "abc`); !errors.Is(err, ErrUnterminatedQuote) {
		t.Fatalf("got %v, want ErrUnterminatedQuote", err)
	}
}
