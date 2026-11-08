// Copyright 2026 The nut-exporter Authors
// SPDX-License-Identifier: Apache-2.0

// Package nut is a read-only client for the NUT network protocol (upsd).
package nut

import "errors"

// ErrUnterminatedQuote is returned for a line that ends inside a quoted element.
var ErrUnterminatedQuote = errors.New("nut: unterminated quoted element")

// Tokenize splits one protocol line (without its trailing newline) into elements, following
// NUT's parseconf rules (docs/net-protocol.txt, common/parseconf.c):
//   - unescaped whitespace outside quotes separates elements;
//   - a double quote opens a quoted element only at the start of an element, and the closing
//     quote ends it, so `"one"two` gives `one` and `two`; `""` gives an empty element;
//   - a backslash takes the next byte literally, inside or outside quotes;
//   - an unescaped `=` outside quotes is an element of its own;
//   - an unescaped `#` outside quotes starts a comment.
//
// Unlike parseconf, bytes outside 0x20-0x7f are kept: the caller decides what to do with them.
func Tokenize(line string) ([]string, error) {
	var (
		out    []string
		word   []byte
		inWord bool // collecting an unquoted element
		quoted bool // inside a quoted element
	)
	end := func() {
		out = append(out, string(word))
		word = word[:0]
		inWord = false
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == '\\' {
			if i+1 < len(line) {
				i++
				word = append(word, line[i])
			}
			if !quoted {
				inWord = true
			}
			continue
		}
		if quoted {
			if c == '"' {
				quoted = false
				end()
				continue
			}
			word = append(word, c)
			continue
		}
		switch {
		case c == '#':
			if inWord {
				end()
			}
			return out, nil
		case c == ' ' || c == '\t' || c == '\r' || c == '\v' || c == '\f':
			if inWord {
				end()
			}
		case c == '=':
			if inWord {
				end()
			}
			out = append(out, "=")
		case c == '"' && !inWord:
			quoted = true
		default:
			word = append(word, c)
			inWord = true
		}
	}
	if quoted {
		return nil, ErrUnterminatedQuote
	}
	if inWord {
		end()
	}
	return out, nil
}
