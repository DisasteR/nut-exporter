// Copyright 2026 The nut-exporter Authors
// SPDX-License-Identifier: Apache-2.0

package nut_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DisasteR/nut-exporter/internal/fakeupsd"
	"github.com/DisasteR/nut-exporter/internal/nut"
)

func dial(t *testing.T, s *fakeupsd.Server, o nut.Options) *nut.Conn {
	t.Helper()
	c, err := nut.Dial(context.Background(), s.Addr(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// Catches: go.nut-style parsing (trimmed spaces, numbers, bools) and lost variables.
func TestListVarFixtureIsVerbatim(t *testing.T) {
	lines, err := fakeupsd.LoadListVar("../../testdata/apc-back-ups-rs-1500g.txt")
	if err != nil {
		t.Fatal(err)
	}
	s := fakeupsd.New(t, fakeupsd.Static(map[string][]string{"LIST VAR ups": lines}))
	vars, err := dial(t, s, nut.Options{}).ListVar(context.Background(), "ups")
	if err != nil {
		t.Fatal(err)
	}
	if len(vars) != 43 {
		t.Fatalf("got %d variables, want 43", len(vars))
	}
	got := map[string]string{}
	for _, v := range vars {
		got[v.Name] = v.Value
	}
	for name, want := range map[string]string{
		"device.serial":     "XX0000X00000  ",
		"ups.firmware.aux":  "L5 ",
		"ups.firmware":      "878.L5 .I",
		"ups.productid":     "0002",
		"ups.vendorid":      "051d",
		"ups.beeper.status": "disabled",
		"input.voltage":     "237.0",
		"ups.test.result":   "No test initiated",
	} {
		if got[name] != want {
			t.Errorf("%s = %q, want %q", name, got[name], want)
		}
	}
	if r := s.Requests(); !reflect.DeepEqual(r, []string{"LIST VAR ups"}) {
		t.Fatalf("requests %q", r)
	}
}

// Catches: treating ERR as a broken stream, or ignoring it.
func TestErrAnswerKeepsConnection(t *testing.T) {
	s := fakeupsd.New(t, fakeupsd.Static(map[string][]string{
		"LIST VAR ups":      {"ERR DATA-STALE"},
		"GET NUMLOGINS ups": {"NUMLOGINS ups 2"},
	}))
	c := dial(t, s, nut.Options{})
	_, err := c.ListVar(context.Background(), "ups")
	var ne *nut.Error
	if !errors.As(err, &ne) || ne.Word != "DATA-STALE" {
		t.Fatalf("got %v, want ERR DATA-STALE", err)
	}
	if c.Broken() {
		t.Fatal("connection closed after an ERR answer")
	}
	n, err := c.NumLogins(context.Background(), "ups")
	if err != nil || n != 2 {
		t.Fatalf("NumLogins = %d, %v", n, err)
	}
	if s.Connections() != 1 {
		t.Fatalf("%d connections, want 1", s.Connections())
	}
}

// Catches: unbounded waits, and reuse of a stream left out of sync.
func TestMissingEndTimesOutAndBreaks(t *testing.T) {
	s := fakeupsd.New(t, func(string) fakeupsd.Response {
		return fakeupsd.Response{Lines: []string{"BEGIN LIST VAR ups", `VAR ups a "1"`}}
	})
	c := dial(t, s, nut.Options{CommandTimeout: 200 * time.Millisecond})
	start := time.Now()
	_, err := c.ListVar(context.Background(), "ups")
	if err == nil {
		t.Fatal("no error without END")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %v with a 200ms command timeout", d)
	}
	if !c.Broken() {
		t.Fatal("connection still usable after a timeout")
	}
	if _, err := c.ListVar(context.Background(), "ups"); !errors.Is(err, nut.ErrClosed) {
		t.Fatalf("reuse: got %v, want ErrClosed", err)
	}
}

// Catches: a cancelled scrape left waiting for the command timeout.
func TestContextCancelUnblocks(t *testing.T) {
	s := fakeupsd.New(t, func(string) fakeupsd.Response { return fakeupsd.Response{Hang: true} })
	c := dial(t, s, nut.Options{CommandTimeout: 10 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.ListUPS(ctx); err == nil {
		t.Fatal("no error")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %v after a 100ms context", d)
	}
}

// Catches: unbounded memory on a hostile or broken server.
func TestLimits(t *testing.T) {
	long := `VAR ups a "` + strings.Repeat("x", 300) + `"`
	s := fakeupsd.New(t, fakeupsd.Static(map[string][]string{
		"LIST VAR big":  {"BEGIN LIST VAR big", strings.Replace(long, "ups", "big", 1), "END LIST VAR big"},
		"LIST VAR many": append(append([]string{"BEGIN LIST VAR many"}, strings.Split(strings.Repeat(`VAR many a "0123456789"`+"\n", 20), "\n")[:20]...), "END LIST VAR many"),
	}))
	c := dial(t, s, nut.Options{MaxLine: 256})
	if _, err := c.ListVar(context.Background(), "big"); !errors.Is(err, nut.ErrTooLarge) {
		t.Fatalf("long line: got %v, want ErrTooLarge", err)
	}
	c = dial(t, s, nut.Options{MaxResponse: 200})
	if _, err := c.ListVar(context.Background(), "many"); !errors.Is(err, nut.ErrTooLarge) {
		t.Fatalf("large answer: got %v, want ErrTooLarge", err)
	}
	c = dial(t, s, nut.Options{MaxResponse: 1000})
	if v, err := c.ListVar(context.Background(), "many"); err != nil || len(v) != 20 {
		t.Fatalf("answer within limits: %d vars, %v", len(v), err)
	}
}

// Catches: accepting answers that do not match the request.
func TestProtocolMismatch(t *testing.T) {
	s := fakeupsd.New(t, fakeupsd.Static(map[string][]string{
		"LIST VAR a":      {"BEGIN LIST VAR b", "END LIST VAR b"},
		"LIST VAR c":      {"BEGIN LIST VAR c", `VAR other x "1"`, "END LIST VAR c"},
		"LIST VAR d":      {"BEGIN LIST VAR d", `VAR d x "unterminated`, "END LIST VAR d"},
		"GET NUMLOGINS e": {"NUMLOGINS other 1"},
		"GET NUMLOGINS f": {"NUMLOGINS f many"},
	}))
	for _, tc := range []struct {
		name string
		f    func(*nut.Conn) error
	}{
		{"wrong BEGIN", func(c *nut.Conn) error { _, err := c.ListVar(context.Background(), "a"); return err }},
		{"item of another UPS", func(c *nut.Conn) error { _, err := c.ListVar(context.Background(), "c"); return err }},
		{"unterminated quote", func(c *nut.Conn) error { _, err := c.ListVar(context.Background(), "d"); return err }},
		{"GET echo mismatch", func(c *nut.Conn) error { _, err := c.NumLogins(context.Background(), "e"); return err }},
		{"non-numeric logins", func(c *nut.Conn) error { _, err := c.NumLogins(context.Background(), "f"); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := dial(t, s, nut.Options{})
			if err := tc.f(c); !errors.Is(err, nut.ErrProtocol) {
				t.Fatalf("got %v, want ErrProtocol", err)
			}
			if !c.Broken() {
				t.Fatal("connection kept after a protocol error")
			}
		})
	}
}

// Catches: wrong parsing of each answer shape, and unquoted request arguments.
func TestAnswerShapes(t *testing.T) {
	s := fakeupsd.New(t, fakeupsd.Static(map[string][]string{
		"VER":                      {"Network UPS Tools upsd 2.7.4 - http://www.networkupstools.org/"},
		"NETVER":                   {"1.2"},
		"LIST UPS":                 {"BEGIN LIST UPS", `UPS ups "Back \"RS\""`, `UPS b ""`, "END LIST UPS"},
		"LIST CMD ups":             {"BEGIN LIST CMD ups", "CMD ups beeper.enable", "CMD ups load.off", "END LIST CMD ups"},
		"LIST CLIENT ups":          {"BEGIN LIST CLIENT ups", "CLIENT ups 127.0.0.1", "CLIENT ups 127.0.0.1", "END LIST CLIENT ups"},
		"LIST RW ups":              {"BEGIN LIST RW ups", `RW ups battery.mfr.date "2026/11/06"`, "END LIST RW ups"},
		"LIST ENUM ups x":          {"BEGIN LIST ENUM ups x", `ENUM ups x "low"`, `ENUM ups x "high"`, "END LIST ENUM ups x"},
		"LIST RANGE ups x":         {"BEGIN LIST RANGE ups x", `RANGE ups x "100" "120"`, `RANGE ups x "200" "240"`, "END LIST RANGE ups x"},
		"GET TYPE ups x":           {"TYPE ups x RW ENUM STRING:16"},
		"GET DESC ups x":           {`DESC ups x "Some \\ text"`},
		"GET CMDDESC ups load.off": {`CMDDESC ups load.off "Turn off the load immediately"`},
		`LIST VAR "my ups"`:        {`BEGIN LIST VAR "my ups"`, `VAR "my ups" a "1"`, `END LIST VAR "my ups"`},
	}))
	c := dial(t, s, nut.Options{})
	ctx := context.Background()
	check := func(name string, got, want any, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s = %#v, want %#v", name, got, want)
		}
	}
	v, err := c.Version(ctx)
	check("VER", v, "Network UPS Tools upsd 2.7.4 - http://www.networkupstools.org/", err)
	v, err = c.ProtocolVersion(ctx)
	check("NETVER", v, "1.2", err)
	ups, err := c.ListUPS(ctx)
	check("LIST UPS", ups, []nut.UPS{{Name: "ups", Description: `Back "RS"`}, {Name: "b", Description: ""}}, err)
	cmds, err := c.ListCmd(ctx, "ups")
	check("LIST CMD", cmds, []string{"beeper.enable", "load.off"}, err)
	cl, err := c.ListClient(ctx, "ups")
	check("LIST CLIENT", cl, []string{"127.0.0.1", "127.0.0.1"}, err)
	rw, err := c.ListRW(ctx, "ups")
	check("LIST RW", rw, []nut.Var{{Name: "battery.mfr.date", Value: "2026/11/06"}}, err)
	en, err := c.ListEnum(ctx, "ups", "x")
	check("LIST ENUM", en, []string{"low", "high"}, err)
	rg, err := c.ListRange(ctx, "ups", "x")
	check("LIST RANGE", rg, []nut.Range{{Min: "100", Max: "120"}, {Min: "200", Max: "240"}}, err)
	ty, err := c.Type(ctx, "ups", "x")
	check("GET TYPE", ty, []string{"RW", "ENUM", "STRING:16"}, err)
	d, err := c.Description(ctx, "ups", "x")
	check("GET DESC", d, `Some \ text`, err)
	d, err = c.CommandDescription(ctx, "ups", "load.off")
	check("GET CMDDESC", d, "Turn off the load immediately", err)
	vs, err := c.ListVar(ctx, "my ups")
	check("quoted UPS name", vs, []nut.Var{{Name: "a", Value: "1"}}, err)
	for _, r := range s.Requests() {
		if fakeupsd.Mutating(r) {
			t.Fatalf("mutating request %q", r)
		}
	}
}
