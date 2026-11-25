// Copyright 2026 Benjamin Saiz
// SPDX-License-Identifier: Apache-2.0

// Command fakesrv serves every dump of a NUT DDL checkout (github.com/networkupstools/nut-ddl) as
// one UPS of a single upsd, answering like upsd 2.7.4: dump i in sorted path order is UPS
// d<iiii>. With -base, dump i is also served alone on port base+i, for collectors that fail a
// whole server on one bad UPS. It writes the UPS name -> dump file and parsed variables to -index.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	ddlLine    = regexp.MustCompile(`^([A-Za-z0-9._-]+):(?: (.*))?$`)
	ddlComment = regexp.MustCompile(`[ \t]+#.*$`) // dummy-ups: # after a blank starts a comment
)

type variable struct{ Name, Value string }

type dump struct {
	UPS  string     `json:"ups"`
	File string     `json:"file"`
	Vars []variable `json:"vars"`
}

// read parses `name: value` lines; upsd holds one value per name, so the last one wins.
func read(path string) ([]variable, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	pos := map[string]int{}
	var out []variable
	s := bufio.NewScanner(f)
	for s.Scan() {
		m := ddlLine.FindStringSubmatch(ddlComment.ReplaceAllString(strings.TrimRight(s.Text(), "\r"), ""))
		if m == nil {
			continue
		}
		if i, ok := pos[m[1]]; ok {
			out[i].Value = m[2]
			continue
		}
		pos[m[1]] = len(out)
		out = append(out, variable{m[1], m[2]})
	}
	return out, s.Err()
}

// quote escapes a value as upsd does: only \ and ".
func quote(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}

type server struct{ byName map[string]*dump }

func (s *server) answer(line string) []string {
	w := strings.Fields(line)
	list := func(items []string) []string {
		h := strings.Join(w[1:], " ")
		return append(append([]string{"BEGIN LIST " + h}, items...), "END LIST "+h)
	}
	switch line {
	case "VER":
		return []string{"Network UPS Tools upsd 2.7.4 - http://www.networkupstools.org/"}
	case "NETVER":
		return []string{"1.2"}
	case "LOGOUT":
		return []string{"OK Goodbye"}
	case "LIST UPS":
		names := make([]string, 0, len(s.byName))
		for n := range s.byName {
			names = append(names, n)
		}
		sort.Strings(names)
		l := make([]string, 0, len(names))
		for _, n := range names {
			l = append(l, "UPS "+n+` "Unavailable"`)
		}
		return list(l)
	}
	if len(w) < 3 {
		return []string{"ERR UNKNOWN-COMMAND"}
	}
	d := s.byName[w[2]]
	if d == nil {
		return []string{"ERR UNKNOWN-UPS"}
	}
	value := func(n string) (string, bool) {
		for _, v := range d.Vars {
			if v.Name == n {
				return v.Value, true
			}
		}
		return "", false
	}
	known := len(w) >= 4
	if known {
		_, known = value(w[3])
	}
	switch w[0] + " " + w[1] {
	case "LIST VAR":
		l := make([]string, 0, len(d.Vars))
		for _, v := range d.Vars {
			l = append(l, "VAR "+d.UPS+" "+v.Name+" "+quote(v.Value))
		}
		return list(l)
	case "LIST CMD", "LIST CLIENT", "LIST RW":
		return list(nil)
	case "LIST ENUM", "LIST RANGE":
		if !known {
			return []string{"ERR VAR-NOT-SUPPORTED"}
		}
		return list(nil)
	case "GET UPSDESC":
		return []string{"UPSDESC " + d.UPS + ` "Unavailable"`}
	case "GET NUMLOGINS":
		return []string{"NUMLOGINS " + d.UPS + " 0"}
	case "GET TYPE": // upsd answers NUMBER for a variable without type flags
		if !known {
			return []string{"ERR VAR-NOT-SUPPORTED"}
		}
		return []string{"TYPE " + d.UPS + " " + w[3] + " NUMBER"}
	case "GET DESC":
		if !known {
			return []string{"ERR VAR-NOT-SUPPORTED"}
		}
		return []string{"DESC " + d.UPS + " " + w[3] + ` "Description unavailable"`}
	case "GET VAR":
		if !known {
			return []string{"ERR VAR-NOT-SUPPORTED"}
		}
		v, _ := value(w[3])
		return []string{"VAR " + d.UPS + " " + w[3] + " " + quote(v)}
	}
	return []string{"ERR UNKNOWN-COMMAND"}
}

func (s *server) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if _, err := c.Write([]byte(strings.Join(s.answer(line), "\n") + "\n")); err != nil || line == "LOGOUT" {
			return
		}
	}
}

func main() {
	root := flag.String("root", "", "nut-ddl checkout")
	listen := flag.String("listen", "127.0.0.1:34930", "listen address")
	index := flag.String("index", "", "index JSON output")
	base := flag.Int("base", 0, "if set, also serve dump i alone on 127.0.0.1:base+i")
	flag.Parse()
	dir, err := filepath.EvalSymlinks(*root) // WalkDir does not descend into a symlinked root
	if err != nil {
		log.Fatal(err)
	}
	var files []string
	err = filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() && (strings.HasSuffix(p, ".dev") || strings.HasSuffix(p, ".nds")) {
			files = append(files, p)
		}
		return err
	})
	if err != nil {
		log.Fatal(err)
	}
	if len(files) == 0 {
		log.Fatalf("no .dev or .nds file under %s", *root)
	}
	sort.Strings(files)
	s := &server{byName: map[string]*dump{}}
	var dumps []*dump
	for i, p := range files {
		vars, err := read(p)
		if err != nil {
			log.Fatal(err)
		}
		rel, _ := filepath.Rel(dir, p)
		d := &dump{UPS: fmt.Sprintf("d%04d", i), File: rel, Vars: vars}
		s.byName[d.UPS] = d
		dumps = append(dumps, d)
	}
	b, err := json.Marshal(dumps)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*index, b, 0o644); err != nil {
		log.Fatal(err)
	}
	if *base > 0 {
		for i, d := range dumps { // bound before "ready" is logged
			l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *base+i))
			if err != nil {
				log.Fatal(err)
			}
			go serveOn(l, &server{byName: map[string]*dump{d.UPS: d}})
		}
	}
	l, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("ready: %d UPSes on %s", len(dumps), *listen)
	serveOn(l, s)
}

func serveOn(l net.Listener, s *server) {
	for {
		c, err := l.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go s.serve(c)
	}
}
