package nut

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"reflect"
	"testing"
)

// specConn uses the real protocol parser without requiring a loopback listener.
func specConn(t *testing.T, request, reply string, opt Options) *Conn {
	t.Helper()
	client, server := net.Pipe()
	opt = opt.withDefaults()
	c := &Conn{conn: client, r: bufio.NewReaderSize(client, opt.MaxLine), opt: opt}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer server.Close()
		got, err := bufio.NewReader(server).ReadString('\n')
		if err != nil {
			t.Errorf("read request: %v", err)
			return
		}
		if got != request {
			t.Errorf("request = %q, want %q", got, request)
			return
		}
		// Early close is expected when the client rejects an oversized answer.
		_, _ = io.WriteString(server, reply)
	}()
	t.Cleanup(func() { c.Close(); server.Close(); <-done })
	return c
}

// Catches: MaxLine ignoring the newline or bufio's minimum buffer size.
func TestSpecMaxLineIncludesNewline(t *testing.T) {
	for _, limit := range []int{3, 4} {
		c := specConn(t, "VER\n", "abc\n", Options{MaxLine: limit})
		got, err := c.Version(context.Background())
		if limit == 3 {
			if !errors.Is(err, ErrTooLarge) || !c.Broken() {
				t.Errorf("limit 3: value=%q err=%v broken=%v", got, err, c.Broken())
			}
		} else if err != nil || got != "abc" || c.Broken() {
			t.Errorf("limit 4: value=%q err=%v broken=%v", got, err, c.Broken())
		}
	}
}

// Catches: response accounting omitting BEGIN, END, carriage returns or newlines.
func TestSpecMaxResponseExactWireBytes(t *testing.T) {
	for _, ending := range []string{"\n", "\r\n"} {
		reply := "BEGIN LIST UPS" + ending + "UPS a \"A\"" + ending + "END LIST UPS" + ending
		for _, limit := range []int{len(reply) - 1, len(reply)} {
			c := specConn(t, "LIST UPS\n", reply, Options{MaxResponse: limit})
			got, err := c.ListUPS(context.Background())
			if limit < len(reply) {
				if !errors.Is(err, ErrTooLarge) || !c.Broken() {
					t.Errorf("ending=%q limit=%d: got=%v err=%v broken=%v", ending, limit, got, err, c.Broken())
				}
			} else if err != nil || !reflect.DeepEqual(got, []UPS{{Name: "a", Description: "A"}}) || c.Broken() {
				t.Errorf("exact size: got=%v err=%v broken=%v", got, err, c.Broken())
			}
		}
	}
}

// Catches: a mid-list ERR losing its NUT word or leaving a desynchronized connection reusable.
func TestSpecMidListError(t *testing.T) {
	c := specConn(t, "LIST VAR a\n", "BEGIN LIST VAR a\nVAR a x \"1\"\nERR DATA-STALE\n", Options{})
	_, err := c.ListVar(context.Background(), "a")
	var ne *Error
	if !errors.As(err, &ne) || ne.Word != "DATA-STALE" {
		t.Errorf("error = %v, want *Error DATA-STALE", err)
	}
	if !c.Broken() {
		t.Error("mid-list error left connection usable")
	}
}
