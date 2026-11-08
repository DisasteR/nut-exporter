// Copyright 2026 The nut-exporter Authors
// SPDX-License-Identifier: Apache-2.0

package fakeupsd

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func dial(t *testing.T, s *Server) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", s.Addr(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if err := c.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return c
}

func send(t *testing.T, c net.Conn, line string) {
	t.Helper()
	if _, err := io.WriteString(c, line); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, r *bufio.Reader, want string) {
	t.Helper()
	got, err := r.ReadString('\n')
	if err != nil || got != want {
		t.Fatalf("read = %q, %v; want %q", got, err, want)
	}
}

// Catches tokenized matching, reordered answers, CR retention and aliased snapshots.
func TestSequential(t *testing.T) {
	s := New(t, Static(map[string][]string{"GET  x": {"one", "two"}, "NEXT": {"three"}}))
	c := dial(t, s)
	r := bufio.NewReader(c)
	send(t, c, "GET  x\r\nNEXT\nunknown\n")
	for _, want := range []string{"one\n", "two\n", "three\n", "ERR UNKNOWN-COMMAND\n"} {
		read(t, r, want)
	}
	want := []string{"GET  x", "NEXT", "unknown"}
	if got := s.Requests(); !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %q", got)
	}
	got := s.Requests()
	got[0] = "changed"
	if !reflect.DeepEqual(s.Requests(), want) {
		t.Fatal("snapshot aliases server")
	}
	if s.Connections() != 1 {
		t.Fatalf("connections = %d", s.Connections())
	}
}

// Catches serialized connections, missing requests and unsynchronized recorder access.
func TestParallel(t *testing.T) {
	const n = 12
	entered := make(chan struct{}, n)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	s := New(t, func(line string) Response { entered <- struct{}{}; <-release; return Response{Lines: []string{line}} })
	defer unblock()
	clients := make([]net.Conn, n)
	for i := range clients {
		clients[i] = dial(t, s)
		send(t, clients[i], fmt.Sprintf("request%d\n", i))
	}
	for range n {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("connections did not run in parallel")
		}
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				s.Requests()
				s.Connections()
			}
		}()
	}
	unblock()
	for i, c := range clients {
		read(t, bufio.NewReader(c), fmt.Sprintf("request%d\n", i))
	}
	wg.Wait()
	if s.Connections() != n {
		t.Fatalf("connections = %d", s.Connections())
	}
	seen := map[string]int{}
	for _, line := range s.Requests() {
		seen[line]++
	}
	if len(seen) != n {
		t.Fatalf("requests = %v", seen)
	}
	for i := range n {
		if seen[fmt.Sprintf("request%d", i)] != 1 {
			t.Fatalf("requests = %v", seen)
		}
	}
}

// Catches skipped delay and closing before or instead of sending lines.
func TestDelayClose(t *testing.T) {
	s := New(t, func(string) Response {
		return Response{Delay: 80 * time.Millisecond, Lines: []string{"last"}, Close: true}
	})
	c := dial(t, s)
	start := time.Now()
	send(t, c, "go\n")
	r := bufio.NewReader(c)
	read(t, r, "last\n")
	if time.Since(start) < 80*time.Millisecond {
		t.Fatal("delay ignored")
	}
	if _, err := r.ReadByte(); err != io.EOF {
		t.Fatalf("close: %v", err)
	}
	s2 := New(t, func(string) Response { return Response{Close: true} })
	c2 := dial(t, s2)
	send(t, c2, "go\n")
	if _, err := bufio.NewReader(c2).ReadByte(); err != io.EOF {
		t.Fatalf("empty close: %v", err)
	}
}

// Catches Hang answering, processing later commands or retaining disconnected clients.
func TestHang(t *testing.T) {
	entered := make(chan struct{})
	s := New(t, func(string) Response { close(entered); return Response{Hang: true, Lines: []string{"wrong"}} })
	c := dial(t, s)
	send(t, c, "hang\nnext\n")
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("handler not called")
	}
	if err := c.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	_, err := bufio.NewReader(c).ReadByte()
	if e, ok := err.(net.Error); !ok || !e.Timeout() {
		t.Fatalf("hang read: %v", err)
	}
	if got := s.Requests(); !reflect.DeepEqual(got, []string{"hang"}) {
		t.Fatalf("requests = %q", got)
	}
	c.Close()
	deadline := time.Now().Add(time.Second)
	for {
		s.mu.Lock()
		active := len(s.active)
		s.mu.Unlock()
		if active == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("hang retained closed client")
		}
		time.Sleep(time.Millisecond)
	}
}

// Catches shutdown waiting for hanging reads or the full response delay.
func TestShutdown(t *testing.T) {
	for _, response := range []Response{{Hang: true}, {Delay: time.Hour}} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			t.Run(fmt.Sprintf("hang=%v", response.Hang), func(t *testing.T) {
				entered := make(chan struct{})
				s := New(t, func(string) Response { close(entered); return response })
				c, err := net.DialTimeout("tcp", s.Addr(), time.Second)
				if err != nil {
					t.Fatal(err)
				}
				// Keep the client open until server cleanup completes.
				defer func() { go func() { <-done; c.Close() }() }()
				send(t, c, "go\n")
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("handler not called")
				}
			})
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("shutdown blocked")
		}
	}
}

// Catches trimming fixture values and dropping or retaining incorrect lines.
func TestLoadListVar(t *testing.T) {
	lines, err := LoadListVar("../../testdata/apc-back-ups-rs-1500g.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 45 || lines[0] != "BEGIN LIST VAR ups" || lines[44] != "END LIST VAR ups" {
		t.Fatalf("fixture framing: %q", lines)
	}
	count := 0
	serial := false
	for _, line := range lines {
		if strings.HasPrefix(line, "VAR ") {
			count++
		}
		if line == `VAR ups device.serial "XX0000X00000  "` {
			serial = true
		}
		if strings.HasPrefix(line, "#") {
			t.Fatal("comment retained")
		}
	}
	if count != 43 || !serial {
		t.Fatalf("count=%d serial=%v", count, serial)
	}
	file := t.TempDir() + "/fixture"
	if err := os.WriteFile(file, []byte("#comment\n\n raw \r\nlast  "), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadListVar(file)
	if err != nil || !reflect.DeepEqual(got, []string{"", " raw \r", "last  "}) {
		t.Fatalf("raw lines = %q, %v", got, err)
	}
	if _, err := LoadListVar(file + "-missing"); err == nil {
		t.Fatal("missing file accepted")
	}
}

// Catches case-sensitive checks, prefix matching and forbidden command omissions.
func TestMutating(t *testing.T) {
	for _, cmd := range []string{"SET", "INSTCMD", "LOGIN", "USERNAME", "PASSWORD", "FSD", "MASTER", "PRIMARY", "STARTTLS"} {
		for _, line := range []string{cmd, strings.ToLower(cmd) + " ups", " \t" + cmd + "\tups"} {
			if !Mutating(line) {
				t.Errorf("allowed %q", line)
			}
		}
	}
	for _, line := range []string{"VER", "NETVER", "PROTVER", "HELP", "GET", "LIST", "LOGOUT", "", " \t", "SETTING x", "GET SET", "LIST LOGIN", "LOGINx"} {
		if Mutating(line) {
			t.Errorf("rejected %q", line)
		}
	}
}
