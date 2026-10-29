package main

import (
	"bufio"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalizeNewlines(t *testing.T) {
	got := string(normalizeNewlines([]byte("a\r.\rMAIL FROM:<x>\r\nb\n")))
	if want := "a\n.\nMAIL FROM:<x>\nb\n"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestCleanTextStripsControls(t *testing.T) {
	if got := cleanText("a\x1b[2Jb\u009bc\x7fd"); got != "a [2Jb c d" {
		t.Fatalf("got %q", got)
	}
}

func TestForgetRemovesQuarantineFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.partial")
	os.WriteFile(p, []byte("data"), 0o600)
	st := &State{Failures: map[string]*Failure{"x": {Partial: p}}}
	st.forget("x")
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("partial file still there: %v", err)
	}
	if len(st.Failures) != 0 {
		t.Fatal("failure record not dropped")
	}
}

func listingClient(t *testing.T, reply string) *popClient {
	t.Helper()
	srv, cli := net.Pipe()
	t.Cleanup(func() { srv.Close(); cli.Close() })
	go func() {
		buf := make([]byte, 64)
		srv.Read(buf) // the UIDL command
		io.WriteString(srv, reply)
	}()
	c := &popClient{conn: cli, idle: 5 * time.Second}
	c.r = bufio.NewReader(readFunc(c.read))
	return c
}

func TestListingRejectsOversizedLine(t *testing.T) {
	c := listingClient(t, "+OK\r\n1 "+strings.Repeat("x", maxListLine+1)+"\r\n.\r\n")
	if _, err := c.listing("UIDL"); !errors.Is(err, errRunaway) {
		t.Fatalf("got %v", err)
	}
}

func TestListingRejectsEndlessListing(t *testing.T) {
	c := listingClient(t, "+OK\r\n"+strings.Repeat("1 a\r\n", maxListLines+1)+".\r\n")
	if _, err := c.listing("UIDL"); !errors.Is(err, errRunaway) {
		t.Fatalf("got %v", err)
	}
}

func TestListingAcceptsNormalReply(t *testing.T) {
	c := listingClient(t, "+OK\r\n1 abc\r\n2 def\r\n.\r\n")
	got, err := c.listing("UIDL")
	if err != nil || len(got) != 2 || got[1][1] != "def" {
		t.Fatalf("got %v, %v", got, err)
	}
}
