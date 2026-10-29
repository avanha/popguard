package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	pass := filepath.Join(dir, "pass")
	os.WriteFile(pass, []byte("hunter2\n"), 0o600)
	cfgPath := filepath.Join(dir, "c.json")
	os.WriteFile(cfgPath, []byte(`{
		"server": "pop.mail.yahoo.com",
		"user": "andrev@yahoo.com",
		"password_file": "`+pass+`",
		"smtp_server": "localhost",
		"deliver_to": ["avanha@localhost"],
		"idle_timeout": "45s",
		"poison_retry_after": "7d",
		"state_file": "`+dir+`/st.json"
	}`), 0o600)

	c, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if c.Server != "pop.mail.yahoo.com:995" || c.SMTPServer != "localhost:25" {
		t.Fatalf("ports not defaulted: %s %s", c.Server, c.SMTPServer)
	}
	if c.Password != "hunter2" {
		t.Fatalf("password %q", c.Password)
	}
	if c.IdleTimeout.Duration != 45*time.Second || c.PoisonRetryAfter.Duration != 7*24*time.Hour {
		t.Fatalf("durations %v %v", c.IdleTimeout, c.PoisonRetryAfter)
	}
	if c.PoisonThreshold != 2 || c.PoisonAction != "skip" || c.CommitEvery != 10 {
		t.Fatalf("defaults %+v", c)
	}
	if c.QuarantineDir != filepath.Join(dir, "quarantine") {
		t.Fatalf("quarantine %q", c.QuarantineDir)
	}
	if len(c.warnings) != 0 {
		t.Fatalf("unexpected warnings %v", c.warnings)
	}
}

func TestLoadConfigRejectsTypos(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(p, []byte(`{"server":"x","user":"u","password":"p","smtp_server":"l","deliver_to":["a"],"posion_action":"delete"}`), 0o600)
	if _, err := loadConfig(p); err == nil || !strings.Contains(err.Error(), "posion_action") {
		t.Fatalf("typo not caught: %v", err)
	}
}

func TestLoadConfigWarnsOnReadablePassword(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(p, []byte(`{"server":"x","user":"u","password":"p","smtp_server":"l","deliver_to":["a"],"state_file":"/tmp/x.json"}`), 0o644)
	c, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.warnings) == 0 {
		t.Fatal("expected a permissions warning")
	}
}

func TestPasswordFileRelativeToConfig(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "yahoo.pass"), []byte("hunter2\n"), 0o600)
	cfgPath := filepath.Join(dir, "yahoo.json")
	os.WriteFile(cfgPath, []byte(`{
		"server": "pop.example.com", "user": "u", "password_file": "yahoo.pass",
		"smtp_server": "localhost", "deliver_to": ["me@localhost"],
		"state_file": "`+dir+`/st.json"
	}`), 0o600)
	c, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if c.Password != "hunter2" {
		t.Fatalf("password %q", c.Password)
	}
}

func TestConfigPaths(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"b.json", "a.json", ".hidden.json", "notes.txt"} {
		os.WriteFile(filepath.Join(dir, n), []byte("{}"), 0o600)
	}
	os.Mkdir(filepath.Join(dir, "sub.json"), 0o700)
	got, err := configPaths(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || accountName(got[0]) != "a" || accountName(got[1]) != "b" {
		t.Fatalf("got %v", got)
	}
	if one, _ := configPaths(got[1]); len(one) != 1 || one[0] != got[1] {
		t.Fatalf("single file: %v", one)
	}
	if _, err := configPaths(t.TempDir()); err == nil {
		t.Fatal("empty dir should be an error")
	}
}
