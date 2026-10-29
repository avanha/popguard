package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	// errStall: the server stopped sending mid-response. This is the poison signature.
	errStall = errors.New("server stalled")
	// errRunaway: the server sent far more than it advertised, or an absurd line.
	errRunaway = errors.New("runaway response")
)

// popError is a -ERR reply. The connection is still usable after one.
type popError struct{ cmd, msg string }

func (e *popError) Error() string { return fmt.Sprintf("%s: -ERR %s", e.cmd, e.msg) }

func (e *popError) inUse() bool {
	u := strings.ToUpper(e.msg)
	return strings.Contains(u, "IN-USE") || strings.Contains(u, "IN USE") || strings.Contains(u, "LOCK")
}

type msgInfo struct {
	num  int
	uid  string
	size int64
}

// popClient is a deliberately small POP3 client in which every read has a
// deadline: an idle deadline (no bytes for c.idle) and, during RETR, a hard
// deadline for the whole message.
type popClient struct {
	conn  net.Conn
	r     *bufio.Reader
	idle  time.Duration
	hard  time.Time
	trace func(string)
	stop  func() bool
}

type readFunc func([]byte) (int, error)

func (f readFunc) Read(p []byte) (int, error) { return f(p) }

func dialPOP(ctx context.Context, cfg *Config, trace func(string)) (*popClient, error) {
	d := net.Dialer{Timeout: cfg.ConnectTimeout.Duration}
	raw, err := d.DialContext(ctx, "tcp", cfg.Server)
	if err != nil {
		return nil, err
	}
	conn := raw
	if cfg.useTLS() {
		host, _, _ := net.SplitHostPort(cfg.Server)
		tc := tls.Client(raw, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, RootCAs: cfg.tlsRoots})
		hctx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout.Duration)
		err := tc.HandshakeContext(hctx)
		cancel()
		if err != nil {
			raw.Close()
			return nil, fmt.Errorf("TLS handshake with %s: %w", cfg.Server, err)
		}
		conn = tc
	}
	c := &popClient{conn: conn, idle: cfg.IdleTimeout.Duration, trace: trace}
	c.r = bufio.NewReaderSize(readFunc(c.read), 64<<10)
	// Ctrl-C / SIGTERM unblocks any pending read immediately.
	c.stop = context.AfterFunc(ctx, func() { conn.Close() })

	// The greeting gets the connect timeout, not the idle timeout.
	c.hard = time.Now().Add(cfg.ConnectTimeout.Duration)
	_, err = c.status("greeting")
	c.hard = time.Time{}
	if err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (c *popClient) read(p []byte) (int, error) {
	dl := time.Now().Add(c.idle)
	if !c.hard.IsZero() && c.hard.Before(dl) {
		dl = c.hard
	}
	c.conn.SetReadDeadline(dl)
	return c.conn.Read(p)
}

func (c *popClient) Close() {
	if c.stop != nil {
		c.stop()
	}
	c.conn.Close()
}

func (c *popClient) wrapErr(err error) error {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		if !c.hard.IsZero() && !time.Now().Before(c.hard) {
			return fmt.Errorf("%w: response still incomplete when the time limit ran out", errStall)
		}
		return fmt.Errorf("%w: no data from server for %s", errStall, c.idle)
	}
	return err
}

const maxLine = 4 << 20

func (c *popClient) readLine() (string, error) {
	var buf []byte
	for {
		chunk, err := c.r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if err == nil {
			break
		}
		if err == bufio.ErrBufferFull {
			if len(buf) > maxLine {
				return "", fmt.Errorf("%w: line longer than %d bytes", errRunaway, maxLine)
			}
			continue
		}
		// Return the incomplete line too: for a stalled RETR it is evidence.
		return string(buf), c.wrapErr(err)
	}
	s := strings.TrimSuffix(string(buf), "\n")
	return strings.TrimSuffix(s, "\r"), nil
}

func (c *popClient) send(line string) error {
	if c.trace != nil {
		shown := line
		if strings.HasPrefix(strings.ToUpper(line), "PASS ") {
			shown = "PASS ****"
		}
		c.trace("> " + shown)
	}
	c.conn.SetWriteDeadline(time.Now().Add(c.idle))
	_, err := io.WriteString(c.conn, line+"\r\n")
	return c.wrapErr(err)
}

func (c *popClient) status(cmd string) (string, error) {
	line, err := c.readLine()
	if err != nil {
		return "", fmt.Errorf("%s: %w", cmd, err)
	}
	line = cleanText(line) // server text ends up in logs and terminals
	if c.trace != nil {
		c.trace("< " + truncate(line, 200))
	}
	switch {
	case strings.HasPrefix(line, "+OK"):
		return strings.TrimSpace(strings.TrimPrefix(line, "+OK")), nil
	case strings.HasPrefix(line, "-ERR"):
		return "", &popError{cmd: cmd, msg: strings.TrimSpace(strings.TrimPrefix(line, "-ERR"))}
	default:
		return "", fmt.Errorf("%s: unexpected response %q", cmd, truncate(line, 80))
	}
}

func (c *popClient) cmd(name, line string) (string, error) {
	if err := c.send(line); err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return c.status(name)
}

func (c *popClient) login(user, pass string) error {
	if _, err := c.cmd("USER", "USER "+user); err != nil {
		return err
	}
	_, err := c.cmd("PASS", "PASS "+pass)
	return err
}

// Limits for UIDL/LIST replies. They are far beyond any real mailbox; they
// only stop a server from streaming a listing forever. RFC 1939 caps a UIDL
// at 70 characters, so a line over maxListLine is not a listing line.
const (
	maxListLines   = 500_000
	maxListLine    = 512
	listingTimeout = 5 * time.Minute
)

func (c *popClient) listing(name string) ([][2]string, error) {
	if _, err := c.cmd(name, name); err != nil {
		return nil, err
	}
	c.hard = time.Now().Add(listingTimeout)
	defer func() { c.hard = time.Time{} }()
	var out [][2]string
	for {
		l, err := c.readLine()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if l == "." {
			return out, nil
		}
		if len(l) > maxListLine {
			return nil, fmt.Errorf("%s: %w: line of %d bytes", name, errRunaway, len(l))
		}
		if len(out) >= maxListLines {
			return nil, fmt.Errorf("%s: %w: more than %d entries", name, errRunaway, maxListLines)
		}
		f := strings.Fields(l)
		if len(f) < 2 {
			return nil, fmt.Errorf("%s: malformed line %q", name, truncate(l, 80))
		}
		out = append(out, [2]string{f[0], f[1]})
	}
}

// inventory returns every message with its UIDL and size, in server order.
func (c *popClient) inventory() ([]msgInfo, error) {
	u, err := c.listing("UIDL")
	if err != nil {
		return nil, fmt.Errorf("UIDL is required for safe operation: %w", err)
	}
	l, err := c.listing("LIST")
	if err != nil {
		return nil, err
	}
	sizes := make(map[int]int64, len(l))
	for _, p := range l {
		n, err1 := strconv.Atoi(p[0])
		s, err2 := strconv.ParseInt(p[1], 10, 64)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("LIST: bad line %q %q", p[0], p[1])
		}
		sizes[n] = s
	}
	msgs := make([]msgInfo, 0, len(u))
	seen := make(map[string]bool, len(u))
	for _, p := range u {
		n, err := strconv.Atoi(p[0])
		if err != nil {
			return nil, fmt.Errorf("UIDL: bad message number %q", p[0])
		}
		if seen[p[1]] {
			return nil, fmt.Errorf("UIDL: server reported duplicate uid %q; refusing to continue", p[1])
		}
		seen[p[1]] = true
		msgs = append(msgs, msgInfo{num: n, uid: p[1], size: sizes[n]})
	}
	sort.Slice(msgs, func(i, j int) bool { return msgs[i].num < msgs[j].num })
	return msgs, nil
}

// retr downloads one message and returns it dot-unstuffed with LF line
// endings. On failure it also returns whatever arrived, for quarantine.
func (c *popClient) retr(m msgInfo, limit time.Duration, maxBytes int64) ([]byte, error) {
	if _, err := c.cmd("RETR", fmt.Sprintf("RETR %d", m.num)); err != nil {
		return nil, err
	}
	c.hard = time.Now().Add(limit)
	defer func() { c.hard = time.Time{} }()
	var buf bytes.Buffer
	for {
		line, err := c.readLine()
		if err != nil {
			buf.WriteString(line)
			return buf.Bytes(), err
		}
		if line == "." {
			return buf.Bytes(), nil
		}
		buf.WriteString(strings.TrimPrefix(line, "."))
		buf.WriteByte('\n')
		if int64(buf.Len()) > maxBytes {
			return buf.Bytes(), fmt.Errorf("%w: passed %d bytes, server advertised %d", errRunaway, maxBytes, m.size)
		}
	}
}

func (c *popClient) dele(n int) error {
	_, err := c.cmd("DELE", fmt.Sprintf("DELE %d", n))
	return err
}

// quit ends the session; only a successful QUIT makes DELEs permanent.
func (c *popClient) quit() error {
	_, err := c.cmd("QUIT", "QUIT")
	c.Close()
	return err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
