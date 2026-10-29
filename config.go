package main

import (
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Duration accepts "60s", "10m", "7d" or a bare number of seconds in JSON.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		var secs float64
		if err2 := json.Unmarshal(b, &secs); err2 != nil {
			return errors.New(`duration must be a string like "60s" or "7d", or a number of seconds`)
		}
		d.Duration = time.Duration(secs * float64(time.Second))
		return nil
	}
	v, err := parseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func parseDuration(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		days, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err != nil {
			return 0, fmt.Errorf("bad duration %q", s)
		}
		return time.Duration(days * 24 * float64(time.Hour)), nil
	}
	return time.ParseDuration(s)
}

// Config is one POP3 account. Run one popguard per account.
type Config struct {
	Server       string `json:"server"` // host:port; port defaults to 995 (TLS) or 110
	TLS          *bool  `json:"tls"`    // implicit TLS (POP3S); default true
	User         string `json:"user"`
	Password     string `json:"password"`
	PasswordFile string `json:"password_file"` // preferred over password

	// Delivery: exactly one of smtp_server or mda.
	SMTPServer string   `json:"smtp_server"` // e.g. "localhost:25"
	DeliverTo  []string `json:"deliver_to"`  // RCPT TO addresses for SMTP delivery
	MDA        []string `json:"mda"`         // argv; "%F" is replaced by the envelope sender

	Keep bool `json:"keep"` // leave messages on the server (UIDL-tracked)

	StateFile     string `json:"state_file"`
	QuarantineDir string `json:"quarantine_dir"` // partial downloads of poison messages; "none" disables

	ConnectTimeout Duration `json:"connect_timeout"` // dial + TLS + greeting
	IdleTimeout    Duration `json:"idle_timeout"`    // no bytes for this long = server stalled
	MessageTimeout Duration `json:"message_timeout"` // hard cap for one RETR
	DeliverTimeout Duration `json:"deliver_timeout"`

	PoisonThreshold  int      `json:"poison_threshold"`   // failures before a message is poison
	PoisonRetryAfter Duration `json:"poison_retry_after"` // retry poison this long after last failure; 0 = never
	PoisonAction     string   `json:"poison_action"`      // "skip" (leave on server) or "delete"

	CommitEvery    int   `json:"commit_every"` // QUIT/reconnect after this many DELEs so they stick
	MaxStalls      int   `json:"max_stalls"`   // stop fetching after this many failures in one run
	MaxMessageSize int64 `json:"max_message_size"`
	MaxPerRun      int   `json:"max_per_run"` // 0 = unlimited

	InUseRetries int      `json:"in_use_retries"` // login retries when the server says the mailbox is locked
	InUseBackoff Duration `json:"in_use_backoff"`

	path     string
	warnings []string
	tlsRoots *x509.CertPool // tests only; nil = system roots
}

func (c *Config) useTLS() bool { return c.TLS == nil || *c.TLS }

func loadConfig(path string) (*Config, error) {
	path = expandHome(path)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.path = path
	if c.Password != "" && fi.Mode().Perm()&0o077 != 0 {
		c.warnings = append(c.warnings, fmt.Sprintf(
			"%s contains a password but has mode %o; chmod 600 it", path, fi.Mode().Perm()))
	}
	if err := c.finish(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// finish fills defaults and validates. Split out so tests can build configs directly.
func (c *Config) finish() error {
	if c.Server == "" {
		return errors.New("server is required")
	}
	if _, _, err := net.SplitHostPort(c.Server); err != nil {
		port := "995"
		if !c.useTLS() {
			port = "110"
		}
		c.Server = net.JoinHostPort(c.Server, port)
	}
	if c.User == "" {
		return errors.New("user is required")
	}
	if c.PasswordFile != "" {
		p := expandHome(c.PasswordFile)
		if fi, err := os.Stat(p); err == nil && fi.Mode().Perm()&0o077 != 0 {
			c.warnings = append(c.warnings, fmt.Sprintf("%s has mode %o; chmod 600 it", p, fi.Mode().Perm()))
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("password_file: %w", err)
		}
		c.Password = strings.TrimRight(string(b), "\r\n")
	}
	if c.Password == "" {
		return errors.New("password or password_file is required")
	}
	if strings.ContainsAny(c.User+c.Password, "\r\n") {
		return errors.New("user/password must not contain line breaks")
	}

	switch {
	case c.SMTPServer != "" && len(c.MDA) > 0:
		return errors.New("set smtp_server or mda, not both")
	case c.SMTPServer != "":
		if _, _, err := net.SplitHostPort(c.SMTPServer); err != nil {
			c.SMTPServer = net.JoinHostPort(c.SMTPServer, "25")
		}
		if len(c.DeliverTo) == 0 {
			return errors.New("deliver_to is required with smtp_server")
		}
	case len(c.MDA) > 0:
	default:
		return errors.New("set smtp_server (with deliver_to) or mda")
	}

	setDur := func(d *Duration, def time.Duration) {
		if d.Duration == 0 {
			d.Duration = def
		}
	}
	setDur(&c.ConnectTimeout, 30*time.Second)
	setDur(&c.IdleTimeout, 60*time.Second)
	setDur(&c.MessageTimeout, 10*time.Minute)
	setDur(&c.DeliverTimeout, 2*time.Minute)
	setDur(&c.InUseBackoff, 30*time.Second)
	if c.PoisonRetryAfter.Duration < 0 {
		c.PoisonRetryAfter.Duration = 0
	}

	if c.PoisonThreshold <= 0 {
		c.PoisonThreshold = 2
	}
	switch c.PoisonAction {
	case "":
		c.PoisonAction = "skip"
	case "skip", "delete":
	default:
		return fmt.Errorf("poison_action must be \"skip\" or \"delete\", not %q", c.PoisonAction)
	}
	if c.CommitEvery <= 0 {
		c.CommitEvery = 10
	}
	if c.MaxStalls <= 0 {
		c.MaxStalls = 3
	}
	if c.MaxMessageSize <= 0 {
		c.MaxMessageSize = 50 << 20
	}
	if c.InUseRetries < 0 {
		c.InUseRetries = 0
	}

	if c.StateFile == "" {
		base := os.Getenv("XDG_STATE_HOME")
		if base == "" {
			base = filepath.Join(os.Getenv("HOME"), ".local", "state")
		}
		host, _, _ := net.SplitHostPort(c.Server)
		c.StateFile = filepath.Join(base, "popguard", sanitizeToken(c.User+"@"+host)+".json")
	}
	c.StateFile = expandHome(c.StateFile)
	switch c.QuarantineDir {
	case "":
		c.QuarantineDir = filepath.Join(filepath.Dir(c.StateFile), "quarantine")
	case "none":
		c.QuarantineDir = ""
	default:
		c.QuarantineDir = expandHome(c.QuarantineDir)
	}
	return nil
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

func defaultConfigPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		base = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(base, "popguard", "config.json")
}
