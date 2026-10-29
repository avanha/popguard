package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- fake POP3 server -------------------------------------------------------

type fakeMsg struct {
	uid   string
	body  string // LF line endings
	stall int    // >0: send this many bytes of the body, then go silent
}

type fakePOP struct {
	t        *testing.T
	ln       net.Listener
	mu       sync.Mutex
	box      []*fakeMsg
	retrs    map[string]int
	sessions int
}

func wire(body string) string {
	var b strings.Builder
	for _, l := range strings.SplitAfter(body, "\n") {
		if l == "" {
			continue
		}
		l = strings.TrimSuffix(l, "\n")
		if strings.HasPrefix(l, ".") {
			l = "." + l
		}
		b.WriteString(l + "\r\n")
	}
	return b.String()
}

func newFakePOP(t *testing.T, msgs ...*fakeMsg) *fakePOP {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakePOP{t: t, ln: ln, box: msgs, retrs: map[string]int{}}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

func (s *fakePOP) serve(conn net.Conn) {
	defer conn.Close()
	s.mu.Lock()
	s.sessions++
	box := append([]*fakeMsg(nil), s.box...)
	s.mu.Unlock()
	deleted := map[int]bool{}
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	send := func(f string, a ...any) { fmt.Fprintf(w, f+"\r\n", a...); w.Flush() }
	send("+OK fake ready")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return // dropped without QUIT: deletes are discarded
		}
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) == 0 {
			continue
		}
		arg := func() int { n, _ := strconv.Atoi(f[1]); return n }
		switch strings.ToUpper(f[0]) {
		case "USER":
			send("+OK")
		case "PASS":
			if len(f) < 2 || f[1] != "secret" {
				send("-ERR [AUTH] bad password")
				continue
			}
			send("+OK maildrop ready")
		case "UIDL", "LIST":
			send("+OK")
			for i, m := range box {
				if deleted[i] {
					continue
				}
				if strings.ToUpper(f[0]) == "UIDL" {
					send("%d %s", i+1, m.uid)
				} else {
					send("%d %d", i+1, len(wire(m.body)))
				}
			}
			send(".")
		case "RETR":
			m := box[arg()-1]
			s.mu.Lock()
			s.retrs[m.uid]++
			s.mu.Unlock()
			wb := wire(m.body)
			send("+OK %d octets", len(wb))
			if m.stall > 0 {
				w.WriteString(wb[:m.stall])
				w.Flush()
				io.Copy(io.Discard, r) // silence until the client hangs up
				return
			}
			w.WriteString(wb)
			send(".")
		case "DELE":
			deleted[arg()-1] = true
			send("+OK marked")
		case "QUIT":
			s.mu.Lock()
			// Commit: remove what this session marked deleted.
			gone := map[string]bool{}
			for i, m := range box {
				if deleted[i] {
					gone[m.uid] = true
				}
			}
			var nb []*fakeMsg
			for _, m := range s.box {
				if !gone[m.uid] {
					nb = append(nb, m)
				}
			}
			s.box = nb
			s.mu.Unlock()
			send("+OK bye")
			return
		default:
			send("-ERR unknown")
		}
	}
}

func (s *fakePOP) uids() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, m := range s.box {
		out = append(out, m.uid)
	}
	return out
}

func (s *fakePOP) retrCount(uid string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.retrs[uid]
}

// ---- fake SMTP server -------------------------------------------------------

type fakeSMTP struct {
	ln        net.Listener
	mu        sync.Mutex
	msgs      []string
	froms     []string
	fail      bool // reject with 451 once failAfter messages were accepted
	failAfter int
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeSMTP{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

func (s *fakeSMTP) serve(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	send := func(l string) { io.WriteString(conn, l+"\r\n") }
	send("220 fake ESMTP")
	var from string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			send("250 fake")
		case strings.HasPrefix(cmd, "MAIL FROM:"):
			from = strings.TrimSpace(line[len("MAIL FROM:"):])
			send("250 OK")
		case strings.HasPrefix(cmd, "RCPT TO:"):
			send("250 OK")
		case cmd == "DATA":
			send("354 go ahead")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				l = strings.TrimSuffix(strings.TrimSuffix(l, "\n"), "\r")
				if l == "." {
					break
				}
				b.WriteString(strings.TrimPrefix(l, ".") + "\n")
			}
			s.mu.Lock()
			fail := s.fail && len(s.msgs) >= s.failAfter
			if !fail {
				s.msgs = append(s.msgs, b.String())
				s.froms = append(s.froms, from)
			}
			s.mu.Unlock()
			if fail {
				send("451 try again later")
			} else {
				send("250 OK queued")
			}
		case cmd == "QUIT":
			send("221 bye")
			return
		default:
			send("250 OK")
		}
	}
}

func (s *fakeSMTP) received() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.msgs...)
}

// ---- helpers ----------------------------------------------------------------

func msg(uid string) *fakeMsg {
	return &fakeMsg{uid: uid, body: fmt.Sprintf(
		"Return-Path: <sender-%s@example.com>\nFrom: Sender %s <s@example.com>\nSubject: message %s\n\nbody of %s\n", uid, uid, uid, uid)}
}

func poison(uid string) *fakeMsg {
	m := msg(uid)
	m.stall = 60 // through Return-Path and part of From, like Yahoo
	return m
}

func testConfig(t *testing.T, pop *fakePOP, smtpSrv *fakeSMTP) *Config {
	f := false
	dir := t.TempDir()
	cfg := &Config{
		Server:     pop.ln.Addr().String(),
		TLS:        &f,
		User:       "andrev@yahoo.com",
		Password:   "secret",
		SMTPServer: smtpSrv.ln.Addr().String(),
		DeliverTo:  []string{"avanha@localhost"},
		StateFile:  dir + "/state.json",
	}
	cfg.IdleTimeout.Duration = 200 * time.Millisecond
	cfg.MessageTimeout.Duration = 2 * time.Second
	cfg.ConnectTimeout.Duration = 2 * time.Second
	cfg.InUseRetries = 0
	if err := cfg.finish(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func runOnce(t *testing.T, cfg *Config) (*runner, error) {
	t.Helper()
	st, err := loadState(cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	logger := log.New(&out, "", 0)
	r := newRunner(cfg, st, logger, testing.Verbose())
	err = r.run(context.Background())
	t.Logf("run log:\n%s", out.String())
	return r, err
}

func uidsOf(msgs []string) []string {
	var out []string
	for _, m := range msgs {
		i := strings.Index(m, "Subject: message ")
		out = append(out, strings.Fields(m[i+len("Subject: message "):])[0])
	}
	return out
}

func eq(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
}

// ---- tests ------------------------------------------------------------------

func TestHappyPath(t *testing.T) {
	pop := newFakePOP(t, msg("a"), msg("b"), msg("c"))
	sm := newFakeSMTP(t)
	cfg := testConfig(t, pop, sm)
	r, err := runOnce(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "delivered", uidsOf(sm.received()), []string{"a", "b", "c"})
	eq(t, "left on server", pop.uids(), nil)
	if r.stats.delivered != 3 {
		t.Fatalf("stats %+v", r.stats)
	}
	if sm.froms[0] != "<sender-a@example.com>" {
		t.Fatalf("envelope from = %q", sm.froms[0])
	}
	if !strings.HasPrefix(sm.received()[0], "Received: from 127.0.0.1") {
		t.Fatalf("missing Received header:\n%s", sm.received()[0])
	}
}

func TestPoisonMessageIsIsolatedThenSkipped(t *testing.T) {
	pop := newFakePOP(t, msg("a"), poison("bad"), msg("c"))
	sm := newFakeSMTP(t)
	cfg := testConfig(t, pop, sm)

	// Run 1: a and c get through, the bad one fails once.
	if _, err := runOnce(t, cfg); err != nil {
		t.Fatal(err)
	}
	eq(t, "delivered after run 1", uidsOf(sm.received()), []string{"a", "c"})
	eq(t, "left on server", pop.uids(), []string{"bad"})
	st, _ := loadState(cfg.StateFile)
	f := st.Failures["bad"]
	if f == nil || f.Count != 1 {
		t.Fatalf("failure record after run 1: %+v", f)
	}
	if !strings.Contains(f.Hint, "From: Sender bad") {
		t.Fatalf("hint should name the sender from the partial download: %q", f.Hint)
	}
	if b, err := os.ReadFile(f.Partial); err != nil || !strings.HasPrefix(string(b), "Return-Path:") {
		t.Fatalf("quarantine file %q: %v %q", f.Partial, err, b)
	}

	// Run 2: retried once more, fails again, now poison.
	runOnce(t, cfg)
	if n := pop.retrCount("bad"); n != 2 {
		t.Fatalf("RETR count after run 2 = %d, want 2", n)
	}

	// Run 3: never even asked for.
	r, err := runOnce(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if n := pop.retrCount("bad"); n != 2 {
		t.Fatalf("poison message was retrieved again (RETR count %d)", n)
	}
	if r.stats.poisonSkipped != 1 {
		t.Fatalf("stats %+v", r.stats)
	}
	eq(t, "no duplicates", uidsOf(sm.received()), []string{"a", "c"})
}

func TestPoisonDeleteAction(t *testing.T) {
	pop := newFakePOP(t, poison("bad"), msg("ok"))
	sm := newFakeSMTP(t)
	cfg := testConfig(t, pop, sm)
	cfg.PoisonAction = "delete"
	runOnce(t, cfg)
	runOnce(t, cfg)
	eq(t, "still there before it's poison", pop.uids(), []string{"bad"})
	runOnce(t, cfg)
	eq(t, "poison deleted", pop.uids(), nil)
	if n := pop.retrCount("bad"); n != 2 {
		t.Fatalf("RETR count %d", n)
	}
}

func TestPoisonRetryAfter(t *testing.T) {
	pop := newFakePOP(t, poison("bad"))
	sm := newFakeSMTP(t)
	cfg := testConfig(t, pop, sm)
	cfg.PoisonThreshold = 1
	cfg.PoisonRetryAfter.Duration = time.Hour
	runOnce(t, cfg)
	runOnce(t, cfg)
	if n := pop.retrCount("bad"); n != 1 {
		t.Fatalf("RETR count %d, want 1 (skipped within retry window)", n)
	}
	// Pretend the last failure was long ago.
	st, _ := loadState(cfg.StateFile)
	st.Failures["bad"].LastTry = time.Now().Add(-2 * time.Hour)
	st.save()
	runOnce(t, cfg)
	if n := pop.retrCount("bad"); n != 2 {
		t.Fatalf("RETR count %d, want 2 (retry window passed)", n)
	}
}

// The scenario that bit us with fetchmail: a stall after several successful
// deliveries. The aborted session's DELEs never commit, but nothing may be
// delivered twice.
func TestNoDuplicatesAfterAbortedSession(t *testing.T) {
	pop := newFakePOP(t, msg("1"), msg("2"), msg("3"), poison("4"), msg("5"))
	sm := newFakeSMTP(t)
	cfg := testConfig(t, pop, sm)
	cfg.CommitEvery = 2
	r, err := runOnce(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "delivered once each", uidsOf(sm.received()), []string{"1", "2", "3", "5"})
	eq(t, "only poison left", pop.uids(), []string{"4"})
	if r.stats.redeleted != 1 { // message 3: delivered, then its session died
		t.Fatalf("stats %+v", r.stats)
	}
}

func TestDeliveredButNotDeletedFromPreviousRun(t *testing.T) {
	pop := newFakePOP(t, msg("a"), msg("b"))
	sm := newFakeSMTP(t)
	cfg := testConfig(t, pop, sm)
	st, _ := loadState(cfg.StateFile)
	st.Delivered["a"] = time.Now()
	st.save()
	runOnce(t, cfg)
	eq(t, "a not redelivered", uidsOf(sm.received()), []string{"b"})
	eq(t, "both gone", pop.uids(), nil)
}

func TestKilledMidDownloadCountsAsFailure(t *testing.T) {
	pop := newFakePOP(t, msg("a"))
	sm := newFakeSMTP(t)
	cfg := testConfig(t, pop, sm)
	cfg.PoisonThreshold = 1
	st, _ := loadState(cfg.StateFile)
	st.Retrieving = "a" // as if SIGKILLed during RETR
	st.save()
	runOnce(t, cfg)
	if n := pop.retrCount("a"); n != 0 {
		t.Fatalf("message that killed the last run was fetched again")
	}
}

func TestDeliveryFailureLeavesMailAndCommitsEarlierDeletes(t *testing.T) {
	pop := newFakePOP(t, msg("a"), msg("b"), msg("c"))
	sm := newFakeSMTP(t)
	sm.mu.Lock()
	sm.fail, sm.failAfter = true, 1 // MTA takes one message, then starts 451ing
	sm.mu.Unlock()
	cfg := testConfig(t, pop, sm)
	if _, err := runOnce(t, cfg); err == nil {
		t.Fatal("expected a delivery error")
	}
	eq(t, "a delivered", uidsOf(sm.received()), []string{"a"})
	eq(t, "a's delete committed, rest untouched", pop.uids(), []string{"b", "c"})
	st, _ := loadState(cfg.StateFile)
	if len(st.Failures) != 0 {
		t.Fatalf("an MTA problem must not mark messages as poison: %+v", st.Failures)
	}

	sm.mu.Lock()
	sm.fail = false
	sm.mu.Unlock()
	if _, err := runOnce(t, cfg); err != nil {
		t.Fatal(err)
	}
	eq(t, "delivered after the MTA recovers", uidsOf(sm.received()), []string{"a", "b", "c"})
	eq(t, "mailbox empty", pop.uids(), nil)
}

func TestMaxStallsStopsFetchingButCommits(t *testing.T) {
	pop := newFakePOP(t, msg("a"), poison("x"), poison("y"), msg("b"))
	sm := newFakeSMTP(t)
	cfg := testConfig(t, pop, sm)
	cfg.MaxStalls = 2
	runOnce(t, cfg)
	eq(t, "a delivered, b waits", uidsOf(sm.received()), []string{"a"})
	eq(t, "a's delete committed", pop.uids(), []string{"x", "y", "b"})
}

func TestDotStuffingRoundTrip(t *testing.T) {
	m := msg("dots")
	m.body = "From: x@example.com\nSubject: message dots\n\n.leading dot\n..two dots\n.\nend\n"
	pop := newFakePOP(t, m)
	sm := newFakeSMTP(t)
	cfg := testConfig(t, pop, sm)
	runOnce(t, cfg)
	got := sm.received()
	if len(got) != 1 || !strings.HasSuffix(got[0], m.body) {
		t.Fatalf("body mangled:\n%q\nwant suffix\n%q", got, m.body)
	}
}

func TestEnvelopeFromIsSanitized(t *testing.T) {
	cases := map[string]string{
		"Return-Path: <a@b.com>\n\n":            "a@b.com",
		"Return-Path: <>\n\n":                   "",
		"Return-Path: <-oQ/tmp/x@evil>\n\n":     "",
		"Return-Path: <a b@c>\n\n":              "",
		"From: x\n\nReturn-Path: <body@x>\n":    "",
		"X: y\nreturn-path:   <MiXeD@case>\n\n": "MiXeD@case",
	}
	for in, want := range cases {
		if got := envelopeFrom([]byte(in)); got != want {
			t.Errorf("envelopeFrom(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHeaderHintDecodesSubject(t *testing.T) {
	h := headerHint([]byte("From: \"Kitten Dispatch\" <info@k.com>\nSubject: =?UTF-8?Q?Gourmet=20Cat=20Treats=21?=\n"))
	if h != `From: "Kitten Dispatch" <info@k.com> | Subject: Gourmet Cat Treats!` {
		t.Fatalf("hint %q", h)
	}
}

func TestLockPreventsOverlappingRuns(t *testing.T) {
	p := t.TempDir() + "/s.json"
	unlock, err := lockState(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockState(p); err != errLocked {
		t.Fatalf("second lock: %v", err)
	}
	unlock()
	u2, err := lockState(p)
	if err != nil {
		t.Fatal(err)
	}
	u2()
}

func TestListShowsPoison(t *testing.T) {
	pop := newFakePOP(t, msg("a"), poison("bad"))
	sm := newFakeSMTP(t)
	cfg := testConfig(t, pop, sm)
	cfg.PoisonThreshold = 1
	cfg.Keep = true
	runOnce(t, cfg)

	st, _ := loadState(cfg.StateFile)
	var out strings.Builder
	r := newRunner(cfg, st, log.New(io.Discard, "", 0), false)
	if err := listMessages(context.Background(), r, &out); err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + out.String())
	if !strings.Contains(out.String(), "delivered (kept)") || !strings.Contains(out.String(), "POISON (1x)") {
		t.Fatalf("list output:\n%s", out.String())
	}
	if pop.retrCount("bad") != 1 {
		t.Fatal("-list must not download anything")
	}
}
