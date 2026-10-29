package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"os/exec"
	"strings"
	"time"
)

type deliverer interface {
	Deliver(ctx context.Context, msg []byte, envFrom string) error
}

func newDeliverer(cfg *Config, helo string) deliverer {
	if len(cfg.MDA) > 0 {
		return mdaDeliverer{argv: cfg.MDA}
	}
	return smtpDeliverer{addr: cfg.SMTPServer, rcpts: cfg.DeliverTo, helo: helo, timeout: cfg.DeliverTimeout.Duration}
}

type smtpDeliverer struct {
	addr    string
	rcpts   []string
	helo    string
	timeout time.Duration
}

func (d smtpDeliverer) Deliver(ctx context.Context, msg []byte, envFrom string) error {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", d.addr)
	if err != nil {
		return err
	}
	conn.SetDeadline(time.Now().Add(d.timeout))
	host, _, _ := net.SplitHostPort(d.addr)
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if err := c.Hello(d.helo); err != nil {
		return err
	}
	if err := c.Mail(envFrom); err != nil {
		return fmt.Errorf("MAIL FROM:<%s>: %w", envFrom, err)
	}
	for _, r := range d.rcpts {
		if err := c.Rcpt(r); err != nil {
			return fmt.Errorf("RCPT TO:<%s>: %w", r, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(normalizeNewlines(msg)); err != nil { // DotWriter: LF -> CRLF and dot-stuffing
		return err
	}
	if err := w.Close(); err != nil { // this is where the MTA accepts or rejects
		return err
	}
	return c.Quit()
}

// normalizeNewlines turns every CR or CRLF into LF. The SMTP DotWriter only
// understands CRLF and LF, so a bare CR in the middle of a line would reach
// the MTA as-is, and an MTA that treats it as a line break would see "\r.\r"
// as the end of the message and read the rest as SMTP commands.
func normalizeNewlines(msg []byte) []byte {
	if !bytes.Contains(msg, []byte{'\r'}) {
		return msg
	}
	msg = bytes.ReplaceAll(msg, []byte("\r\n"), []byte{'\n'})
	return bytes.ReplaceAll(msg, []byte{'\r'}, []byte{'\n'})
}

type mdaDeliverer struct{ argv []string }

func (d mdaDeliverer) Deliver(ctx context.Context, msg []byte, envFrom string) error {
	args := make([]string, len(d.argv))
	for i, a := range d.argv {
		args[i] = strings.ReplaceAll(a, "%F", envFrom)
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdin = bytes.NewReader(msg)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", args[0], err, truncate(strings.TrimSpace(string(out)), 300))
	}
	return nil
}

// headerLines yields unfolded-first-line header fields until the blank line.
func headerLines(msg []byte, fn func(name, value string) bool) {
	sc := bufio.NewScanner(bytes.NewReader(msg))
	sc.Buffer(make([]byte, 64<<10), maxLine)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			return
		}
		if line[0] == ' ' || line[0] == '\t' {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if !fn(strings.ToLower(strings.TrimSpace(name)), strings.TrimSpace(value)) {
			return
		}
	}
}

// envelopeFrom takes the sender from Return-Path. The value comes from a
// stranger, so anything that could be an argv option or break a protocol
// line is dropped in favour of the null sender.
func envelopeFrom(msg []byte) string {
	var from string
	headerLines(msg, func(name, value string) bool {
		if name == "return-path" {
			from = strings.TrimSuffix(strings.TrimPrefix(value, "<"), ">")
			return false
		}
		return true
	})
	if from == "" || len(from) > 320 || strings.HasPrefix(from, "-") {
		return ""
	}
	for _, r := range from {
		if r <= ' ' || r == 0x7f || r == '<' || r == '>' {
			return ""
		}
	}
	return from
}

// headerHint pulls From/Subject out of a (possibly truncated) message so the
// log can say which message is poison.
func headerHint(msg []byte) string {
	var from, subj string
	headerLines(msg, func(name, value string) bool {
		switch name {
		case "from":
			if from == "" {
				from = value
			}
		case "subject":
			if subj == "" {
				subj = value
				if dec, err := new(mime.WordDecoder).DecodeHeader(value); err == nil {
					subj = dec
				}
			}
		}
		return from == "" || subj == ""
	})
	var parts []string
	if from != "" {
		parts = append(parts, "From: "+from)
	}
	if subj != "" {
		parts = append(parts, "Subject: "+subj)
	}
	return truncate(cleanText(strings.Join(parts, " | ")), 240)
}

func cleanText(s string) string {
	return strings.Map(func(r rune) rune {
		if r < ' ' || (r >= 0x7f && r <= 0x9f) { // C0, DEL and C1 (U+009B is a CSI)
			return ' '
		}
		return r
	}, s)
}

func receivedHeader(server, host, uid string, rcpts []string, now time.Time) []byte {
	srv, _, _ := net.SplitHostPort(server)
	forPart := ""
	if len(rcpts) == 1 {
		forPart = " for <" + rcpts[0] + ">"
	}
	return []byte(fmt.Sprintf("Received: from %s\n\tby %s with POP3 (popguard) id %s%s;\n\t%s\n",
		srv, host, sanitizeToken(uid), forPart, now.Format(time.RFC1123Z)))
}

// sanitizeToken makes a string safe for file names and header tokens.
func sanitizeToken(s string) string {
	b := []byte(s)
	for i, c := range b {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '.', c == '-', c == '_', c == '@', c == '+', c == '=':
		default:
			b[i] = '_'
		}
	}
	out := string(b)
	out = strings.TrimLeft(out, ".")
	if out == "" {
		out = "_"
	}
	return truncate(out, 100)
}
