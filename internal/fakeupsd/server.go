// Package fakeupsd provides scripted upsd servers for tests.
package fakeupsd

import (
	"bufio"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Response describes the answer to one request.
type Response struct {
	Lines []string
	Delay time.Duration
	Hang  bool
	Close bool
}

// Handler receives a raw request without its line ending.
// Different connections may call it concurrently.
type Handler func(line string) Response

// Server records requests from concurrent connections.
type Server struct {
	listener    net.Listener
	mu          sync.Mutex
	requests    []string
	connections int
	active      map[net.Conn]struct{}
	done        chan struct{}
	wg          sync.WaitGroup
}

// New starts a server and registers its shutdown with t.
func New(t testing.TB, h Handler) *Server {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &Server{listener: l, active: make(map[net.Conn]struct{}), done: make(chan struct{})}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			select {
			case <-s.done:
				s.mu.Unlock()
				c.Close()
				return
			default:
			}
			s.connections++
			s.active[c] = struct{}{}
			s.wg.Add(1)
			s.mu.Unlock()
			go s.serve(c, h)
		}
	}()
	t.Cleanup(s.stop)
	return s
}

func (s *Server) stop() {
	close(s.done)
	s.listener.Close()
	s.mu.Lock()
	for c := range s.active {
		c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Server) serve(c net.Conn, h Handler) {
	defer s.wg.Done()
	defer func() {
		c.Close()
		s.mu.Lock()
		delete(s.active, c)
		s.mu.Unlock()
	}()
	r := bufio.NewReader(c)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		s.mu.Lock()
		s.requests = append(s.requests, line)
		s.mu.Unlock()
		response := h(line)
		if response.Hang {
			io.Copy(io.Discard, r)
			return
		}
		if response.Delay > 0 {
			timer := time.NewTimer(response.Delay)
			select {
			case <-timer.C:
			case <-s.done:
				timer.Stop()
				return
			}
		}
		for _, line := range response.Lines {
			if _, err := io.WriteString(c, line+"\n"); err != nil {
				return
			}
		}
		if response.Close {
			return
		}
	}
}

// Addr returns the listening address.
func (s *Server) Addr() string { return s.listener.Addr().String() }

// Requests returns an independent snapshot in receive order.
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

// Connections returns the number of accepted connections.
func (s *Server) Connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connections
}

// Static matches exact request lines.
func Static(m map[string][]string) Handler {
	return func(line string) Response {
		if lines, ok := m[line]; ok {
			return Response{Lines: lines}
		}
		return Response{Lines: []string{"ERR UNKNOWN-COMMAND"}}
	}
}

// LoadListVar preserves all bytes except newline delimiters and comment lines.
func LoadListVar(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	var out []string
	for _, line := range lines {
		if !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out, nil
}

// Mutating identifies commands forbidden to a read-only client.
func Mutating(line string) bool {
	words := strings.Fields(line)
	if len(words) == 0 {
		return false
	}
	switch strings.ToUpper(words[0]) {
	case "SET", "INSTCMD", "LOGIN", "USERNAME", "PASSWORD", "FSD", "MASTER", "PRIMARY", "STARTTLS":
		return true
	}
	return false
}
