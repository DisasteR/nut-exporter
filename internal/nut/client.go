package nut

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// Options bound a connection. Zero values get the defaults of design §7.
type Options struct {
	ConnectTimeout time.Duration
	CommandTimeout time.Duration
	MaxLine        int // bytes per line, newline included
	MaxResponse    int // bytes per LIST answer
}

func (o Options) withDefaults() Options {
	if o.ConnectTimeout <= 0 {
		o.ConnectTimeout = 2 * time.Second
	}
	if o.CommandTimeout <= 0 {
		o.CommandTimeout = 2 * time.Second
	}
	if o.MaxLine <= 0 {
		o.MaxLine = 8192
	}
	if o.MaxResponse <= 0 {
		o.MaxResponse = 4 << 20
	}
	return o
}

// Error is an `ERR <word> [extra...]` answer from upsd. The connection stays usable.
type Error struct {
	Word  string
	Extra []string
}

func (e *Error) Error() string {
	return "nut: ERR " + strings.Join(append([]string{e.Word}, e.Extra...), " ")
}

// ErrProtocol reports an answer that does not follow the protocol. The connection is closed.
var ErrProtocol = errors.New("nut: protocol error")

// ErrTooLarge reports a line or LIST answer above the configured limits. The connection is closed.
var ErrTooLarge = errors.New("nut: answer too large")

// ErrClosed is returned by a connection closed after an earlier failure.
var ErrClosed = errors.New("nut: connection closed")

// Conn is a read-only upsd session. It is not safe for concurrent use: upsd answers requests in
// order, one at a time per connection. Any I/O, timeout or protocol failure closes it, since the
// stream may be out of sync; an ERR answer does not.
type Conn struct {
	conn   net.Conn
	r      *bufio.Reader
	opt    Options
	closed bool
	stop   func() bool // releases the context watcher of the running command
}

// Dial connects to upsd at addr (host:port).
func Dial(ctx context.Context, addr string, o Options) (*Conn, error) {
	o = o.withDefaults()
	d := net.Dialer{Timeout: o.ConnectTimeout}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	return &Conn{conn: c, r: bufio.NewReaderSize(c, o.MaxLine), opt: o}, nil
}

// Close closes the connection without LOGOUT: upsd needs none for a read-only session.
func (c *Conn) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	return c.conn.Close()
}

// Broken reports whether the connection was closed after a failure.
func (c *Conn) Broken() bool { return c.closed }

func (c *Conn) fail(err error) error {
	c.done()
	c.Close()
	return err
}

func (c *Conn) send(ctx context.Context, words []string) error {
	if c.closed {
		return ErrClosed
	}
	deadline := time.Now().Add(c.opt.CommandTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := c.conn.SetDeadline(deadline); err != nil {
		return c.fail(err)
	}
	// A cancelled context unblocks the pending read or write at once.
	c.stop = context.AfterFunc(ctx, func() { c.conn.SetDeadline(time.Now()) })
	if _, err := io.WriteString(c.conn, encodeLine(words)); err != nil {
		return c.fail(err)
	}
	return nil
}

func (c *Conn) done() {
	if c.stop != nil {
		c.stop()
		c.stop = nil
	}
}

// readLine reads one line without its newline, bounded by MaxLine (newline included). It also
// returns the number of bytes read from the wire.
func (c *Conn) readLine() (string, int, error) {
	b, err := c.r.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) || len(b) > c.opt.MaxLine {
		return "", 0, c.fail(fmt.Errorf("%w: line longer than %d bytes", ErrTooLarge, c.opt.MaxLine))
	}
	if err != nil {
		return "", 0, c.fail(err)
	}
	return strings.TrimSuffix(string(b[:len(b)-1]), "\r"), len(b), nil
}

// readTokens reads one line and tokenizes it; an ERR line becomes an *Error.
func (c *Conn) readTokens() ([]string, error) {
	tok, _, err := c.readTokensN()
	return tok, err
}

func (c *Conn) readTokensN() ([]string, int, error) {
	line, n, err := c.readLine()
	if err != nil {
		return nil, 0, err
	}
	tok, err := c.tokens(line)
	return tok, n, err
}

// tokens tokenizes one answer line; an ERR line becomes an *Error.
func (c *Conn) tokens(line string) ([]string, error) {
	tok, err := Tokenize(line)
	if err != nil {
		return nil, c.fail(fmt.Errorf("%w: %v", ErrProtocol, err))
	}
	if len(tok) > 0 && tok[0] == "ERR" {
		if len(tok) < 2 {
			return nil, c.fail(fmt.Errorf("%w: bare ERR", ErrProtocol))
		}
		return nil, &Error{Word: tok[1], Extra: tok[2:]}
	}
	return tok, nil
}

// rawLine sends a command whose answer is one untokenized line (VER, NETVER).
func (c *Conn) rawLine(ctx context.Context, cmd string) (string, error) {
	if err := c.send(ctx, []string{cmd}); err != nil {
		return "", err
	}
	defer c.done()
	line, _, err := c.readLine()
	if err != nil {
		return "", err
	}
	if f := strings.Fields(line); len(f) > 0 && f[0] == "ERR" {
		if len(f) < 2 {
			return "", c.fail(fmt.Errorf("%w: bare ERR", ErrProtocol))
		}
		return "", &Error{Word: f[1], Extra: f[2:]}
	}
	return line, nil
}

// get sends GET <args> and returns the answer tokens after the echoed `<kind> <args...>` prefix.
func (c *Conn) get(ctx context.Context, kind string, args ...string) ([]string, error) {
	if err := c.send(ctx, append([]string{"GET", kind}, args...)); err != nil {
		return nil, err
	}
	defer c.done()
	tok, err := c.readTokens()
	if err != nil {
		return nil, err
	}
	want := append([]string{kind}, args...)
	if !hasPrefix(tok, want) {
		return nil, c.fail(fmt.Errorf("%w: GET %s answered %q", ErrProtocol, kind, tok))
	}
	return tok[len(want):], nil
}

// list sends LIST <args> and returns, for each item line, the tokens after `<item> <args[1:]...>`.
// The item word is the list kind (VAR for LIST VAR), with LIST UPS items starting with UPS.
func (c *Conn) list(ctx context.Context, args ...string) ([][]string, error) {
	req := append([]string{"LIST"}, args...)
	if err := c.send(ctx, req); err != nil {
		return nil, err
	}
	defer c.done()
	tok, read, err := c.readTokensN()
	if err != nil {
		return nil, err
	}
	if !equal(tok, append([]string{"BEGIN"}, req...)) {
		return nil, c.fail(fmt.Errorf("%w: expected BEGIN %s, got %q", ErrProtocol, strings.Join(req, " "), tok))
	}
	if read > c.opt.MaxResponse {
		return nil, c.fail(fmt.Errorf("%w: answer larger than %d bytes", ErrTooLarge, c.opt.MaxResponse))
	}
	end := append([]string{"END"}, req...)
	prefix := append([]string{args[0]}, args[1:]...)
	var items [][]string
	for {
		line, n, err := c.readLine()
		if err != nil {
			return nil, err
		}
		if read += n; read > c.opt.MaxResponse {
			return nil, c.fail(fmt.Errorf("%w: answer larger than %d bytes", ErrTooLarge, c.opt.MaxResponse))
		}
		tok, err := c.tokens(line)
		if err != nil {
			// An ERR after BEGIN keeps its NUT word; the stream is out of sync, so the
			// connection is closed either way.
			var nerr *Error
			if errors.As(err, &nerr) {
				c.fail(nerr)
			}
			return nil, err
		}
		if equal(tok, end) {
			return items, nil
		}
		if !hasPrefix(tok, prefix) {
			return nil, c.fail(fmt.Errorf("%w: unexpected line in LIST %s: %q", ErrProtocol, args[0], tok))
		}
		items = append(items, tok[len(prefix):])
	}
}

// UPS is a LIST UPS entry.
type UPS struct{ Name, Description string }

// Var is a LIST VAR or LIST RW entry; Value is the decoded protocol value, unaltered.
type Var struct{ Name, Value string }

// Range is a LIST RANGE entry.
type Range struct{ Min, Max string }

// Version returns the raw VER answer.
func (c *Conn) Version(ctx context.Context) (string, error) { return c.rawLine(ctx, "VER") }

// ProtocolVersion returns the raw NETVER answer.
func (c *Conn) ProtocolVersion(ctx context.Context) (string, error) {
	return c.rawLine(ctx, "NETVER")
}

// ListUPS returns the UPSes declared on the server.
func (c *Conn) ListUPS(ctx context.Context) ([]UPS, error) {
	items, err := c.list(ctx, "UPS")
	if err != nil {
		return nil, err
	}
	out := make([]UPS, 0, len(items))
	for _, it := range items {
		if len(it) != 2 {
			return nil, c.fail(fmt.Errorf("%w: UPS item %q", ErrProtocol, it))
		}
		out = append(out, UPS{Name: it[0], Description: it[1]})
	}
	return out, nil
}

func (c *Conn) listVars(ctx context.Context, kind, ups string) ([]Var, error) {
	items, err := c.list(ctx, kind, ups)
	if err != nil {
		return nil, err
	}
	out := make([]Var, 0, len(items))
	for _, it := range items {
		if len(it) != 2 {
			return nil, c.fail(fmt.Errorf("%w: %s item %q", ErrProtocol, kind, it))
		}
		out = append(out, Var{Name: it[0], Value: it[1]})
	}
	return out, nil
}

// ListVar returns every variable of a UPS.
func (c *Conn) ListVar(ctx context.Context, ups string) ([]Var, error) {
	return c.listVars(ctx, "VAR", ups)
}

// ListRW returns the writable variables of a UPS.
func (c *Conn) ListRW(ctx context.Context, ups string) ([]Var, error) {
	return c.listVars(ctx, "RW", ups)
}

func (c *Conn) listWords(ctx context.Context, kind, ups string) ([]string, error) {
	items, err := c.list(ctx, kind, ups)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		if len(it) != 1 {
			return nil, c.fail(fmt.Errorf("%w: %s item %q", ErrProtocol, kind, it))
		}
		out = append(out, it[0])
	}
	return out, nil
}

// ListCmd returns the instant commands a UPS supports.
func (c *Conn) ListCmd(ctx context.Context, ups string) ([]string, error) {
	return c.listWords(ctx, "CMD", ups)
}

// ListClient returns the address of each session logged in to a UPS, repeats included.
func (c *Conn) ListClient(ctx context.Context, ups string) ([]string, error) {
	return c.listWords(ctx, "CLIENT", ups)
}

// ListEnum returns the allowed values of an enumerated variable.
func (c *Conn) ListEnum(ctx context.Context, ups, name string) ([]string, error) {
	items, err := c.list(ctx, "ENUM", ups, name)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		if len(it) != 1 {
			return nil, c.fail(fmt.Errorf("%w: ENUM item %q", ErrProtocol, it))
		}
		out = append(out, it[0])
	}
	return out, nil
}

// ListRange returns the allowed ranges of a variable.
func (c *Conn) ListRange(ctx context.Context, ups, name string) ([]Range, error) {
	items, err := c.list(ctx, "RANGE", ups, name)
	if err != nil {
		return nil, err
	}
	out := make([]Range, 0, len(items))
	for _, it := range items {
		if len(it) != 2 {
			return nil, c.fail(fmt.Errorf("%w: RANGE item %q", ErrProtocol, it))
		}
		out = append(out, Range{Min: it[0], Max: it[1]})
	}
	return out, nil
}

// NumLogins returns the number of sessions logged in to a UPS.
func (c *Conn) NumLogins(ctx context.Context, ups string) (int, error) {
	tok, err := c.get(ctx, "NUMLOGINS", ups)
	if err != nil {
		return 0, err
	}
	if len(tok) != 1 {
		return 0, c.fail(fmt.Errorf("%w: NUMLOGINS %q", ErrProtocol, tok))
	}
	n, err := strconv.Atoi(tok[0])
	if err != nil || n < 0 {
		return 0, c.fail(fmt.Errorf("%w: NUMLOGINS %q", ErrProtocol, tok[0]))
	}
	return n, nil
}

// Type returns the GET TYPE words of a variable (RW, ENUM, RANGE, STRING:n, NUMBER).
func (c *Conn) Type(ctx context.Context, ups, name string) ([]string, error) {
	tok, err := c.get(ctx, "TYPE", ups, name)
	if err != nil {
		return nil, err
	}
	if len(tok) == 0 {
		return nil, c.fail(fmt.Errorf("%w: empty TYPE", ErrProtocol))
	}
	return tok, nil
}

func (c *Conn) getText(ctx context.Context, kind, ups, name string) (string, error) {
	tok, err := c.get(ctx, kind, ups, name)
	if err != nil {
		return "", err
	}
	if len(tok) != 1 {
		return "", c.fail(fmt.Errorf("%w: %s %q", ErrProtocol, kind, tok))
	}
	return tok[0], nil
}

// Description returns the GET DESC text of a variable.
func (c *Conn) Description(ctx context.Context, ups, name string) (string, error) {
	return c.getText(ctx, "DESC", ups, name)
}

// CommandDescription returns the GET CMDDESC text of a command.
func (c *Conn) CommandDescription(ctx context.Context, ups, cmd string) (string, error) {
	return c.getText(ctx, "CMDDESC", ups, cmd)
}

// encodeLine quotes and escapes words that need it, as pconf_encode does.
func encodeLine(words []string) string {
	var b strings.Builder
	for i, w := range words {
		if i > 0 {
			b.WriteByte(' ')
		}
		if w != "" && !strings.ContainsAny(w, " \t\"\\#=") {
			b.WriteString(w)
			continue
		}
		b.WriteByte('"')
		for j := 0; j < len(w); j++ {
			if w[j] == '"' || w[j] == '\\' {
				b.WriteByte('\\')
			}
			b.WriteByte(w[j])
		}
		b.WriteByte('"')
	}
	b.WriteByte('\n')
	return b.String()
}

func equal(a, b []string) bool {
	return len(a) == len(b) && hasPrefix(a, b)
}

func hasPrefix(tok, prefix []string) bool {
	if len(tok) < len(prefix) {
		return false
	}
	for i := range prefix {
		if tok[i] != prefix[i] {
			return false
		}
	}
	return true
}
