// Copyright 2026 Benjamin Saiz
// SPDX-License-Identifier: Apache-2.0

// Command scrapecheck reads "path<TAB>content type" lines on stdin and prints each file that the
// Prometheus scrape parser rejects for that content type, with the error: Prometheus drops such a
// scrape whole. It is a separate module so that the exporter does not depend on Prometheus.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/textparse"
)

func check(path, contentType string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	p, err := textparse.New(b, contentType, false, labels.NewSymbolTable())
	if err != nil {
		return err
	}
	for {
		if _, err := p.Next(); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
	}
}

func main() {
	s := bufio.NewScanner(os.Stdin)
	for s.Scan() {
		path, contentType, _ := strings.Cut(s.Text(), "\t")
		if err := check(path, contentType); err != nil {
			fmt.Printf("%s\t%v\n", path, strings.ReplaceAll(err.Error(), "\n", " "))
		}
	}
	if err := s.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
