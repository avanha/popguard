package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// How long a delivered uid is remembered after it disappears from the server.
// Guards against a server that briefly hides messages and then shows them again.
const deliveredMemory = 7 * 24 * time.Hour

type runStats struct {
	delivered, redeleted, poisonSkipped, poisonDeleted, failures int
}

type runner struct {
	cfg   *Config
	st    *State
	deliv deliverer
	log   *log.Logger
	trace func(string)
	host  string
	now   func() time.Time

	stats         runStats
	failedThisRun map[string]bool // never retry a uid twice in one run
	noted         map[string]bool // log some things once per run
	fetched       int
	stalls        int
	cleanupOnly   bool // after max_stalls: commit pending DELEs, fetch nothing
}

func newRunner(cfg *Config, st *State, logger *log.Logger, verbose bool) *runner {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "localhost"
	}
	r := &runner{
		cfg: cfg, st: st, log: logger, host: host, now: time.Now,
		deliv:         newDeliverer(cfg, host),
		failedThisRun: map[string]bool{},
		noted:         map[string]bool{},
	}
	if verbose {
		r.trace = func(s string) { logger.Print("pop3 " + s) }
	}
	return r
}

func (r *runner) notef(key, format string, args ...any) {
	if r.noted[key] {
		return
	}
	r.noted[key] = true
	r.log.Printf(format, args...)
}

func (r *runner) run(ctx context.Context) error {
	if err := r.recover(); err != nil {
		return err
	}
	for i := 0; i < 1000; i++ {
		done, err := r.session(ctx)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if r.stalls >= r.cfg.MaxStalls && !r.cleanupOnly {
			r.log.Printf("%d failures this run (max_stalls); committing pending deletes and stopping until next run", r.stalls)
			r.cleanupOnly = true
		}
	}
	return errors.New("gave up after 1000 sessions; something is looping")
}

// recover handles a previous run that was killed mid-message.
func (r *runner) recover() error {
	if uid := r.st.Retrieving; uid != "" {
		r.log.Printf("previous run died while downloading uid %s; counting that as a failure", uid)
		r.recordFailure(msgInfo{uid: uid, size: r.st.RetrievingSize}, "previous run died while downloading this message", nil)
		r.st.Retrieving, r.st.RetrievingSize = "", 0
	}
	if uid := r.st.Delivering; uid != "" {
		r.log.Printf("previous run died while delivering uid %s; it will be fetched again (possible duplicate)", uid)
		r.st.Delivering = ""
	}
	return r.st.save()
}

func (r *runner) connect(ctx context.Context) (*popClient, error) {
	for attempt := 0; ; attempt++ {
		c, err := dialPOP(ctx, r.cfg, r.trace)
		if err != nil {
			return nil, fmt.Errorf("connect %s: %w", r.cfg.Server, err)
		}
		err = c.login(r.cfg.User, r.cfg.Password)
		if err == nil {
			return c, nil
		}
		c.Close()
		var pe *popError
		if errors.As(err, &pe) && pe.inUse() && attempt < r.cfg.InUseRetries {
			// Usually our own abandoned session still holding the maildrop lock.
			r.log.Printf("mailbox locked (%s); retrying in %s", pe.msg, r.cfg.InUseBackoff.Duration)
			select {
			case <-time.After(r.cfg.InUseBackoff.Duration):
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return nil, fmt.Errorf("login: %w", err)
	}
}

// prune forgets state for messages no longer on the server.
func (r *runner) prune(msgs []msgInfo) {
	present := make(map[string]bool, len(msgs))
	for _, m := range msgs {
		present[m.uid] = true
	}
	now := r.now()
	for u, when := range r.st.Delivered {
		if !present[u] && now.Sub(when) > deliveredMemory {
			delete(r.st.Delivered, u)
		}
	}
	for u := range r.st.Failures {
		if !present[u] {
			r.st.forget(u)
		}
	}
}

// session runs one POP3 connection. done=false means "reconnect and keep going".
func (r *runner) session(ctx context.Context) (done bool, err error) {
	c, err := r.connect(ctx)
	if err != nil {
		return false, err
	}
	open := true
	defer func() {
		if open {
			c.Close()
		}
	}()
	finish := func(done bool) (bool, error) {
		open = false
		if err := c.quit(); err != nil {
			r.log.Printf("QUIT failed (%v); deletes from this session will be redone next time", err)
		}
		return done, nil
	}

	msgs, err := c.inventory()
	if err != nil {
		return false, err
	}
	r.prune(msgs)
	if err := r.st.save(); err != nil {
		return false, err
	}

	deles := 0
	afterDele := func() (stop bool) {
		deles++
		return deles >= r.cfg.CommitEvery
	}

	for _, m := range msgs {
		if err := ctx.Err(); err != nil {
			return false, err
		}

		if _, ok := r.st.Delivered[m.uid]; ok {
			if r.cfg.Keep {
				continue
			}
			// Delivered earlier but the DELE never committed (session died before QUIT).
			if err := c.dele(m.num); err != nil {
				return false, err
			}
			r.stats.redeleted++
			if afterDele() {
				return finish(false)
			}
			continue
		}

		if r.failedThisRun[m.uid] {
			continue
		}

		if f := r.st.Failures[m.uid]; f != nil && f.Count >= r.cfg.PoisonThreshold {
			since := r.now().Sub(f.LastTry)
			retry := r.cfg.PoisonRetryAfter.Duration > 0 && since >= r.cfg.PoisonRetryAfter.Duration
			if !retry {
				if r.cfg.PoisonAction == "delete" {
					if err := c.dele(m.num); err != nil {
						return false, err
					}
					r.stats.poisonDeleted++
					r.log.Printf("POISON deleted: uid %s, %d bytes, %d failures (%s) %s",
						m.uid, m.size, f.Count, f.LastError, f.Hint)
					if afterDele() {
						return finish(false)
					}
				} else {
					if !r.noted["poison:"+m.uid] {
						r.stats.poisonSkipped++
					}
					r.notef("poison:"+m.uid, "POISON skipped: uid %s, %d bytes, %d failures, last %s ago (%s) %s",
						m.uid, m.size, f.Count, since.Round(time.Minute), f.LastError, f.Hint)
				}
				continue
			}
			r.notef("retry:"+m.uid, "retrying poison uid %s (last failure %s ago)", m.uid, since.Round(time.Minute))
		}

		if r.cleanupOnly {
			continue
		}
		if m.size > r.cfg.MaxMessageSize {
			r.notef("big:"+m.uid, "skipping uid %s: %d bytes is over max_message_size", m.uid, m.size)
			continue
		}
		if r.cfg.MaxPerRun > 0 && r.fetched >= r.cfg.MaxPerRun {
			r.notef("maxperrun", "reached max_per_run (%d); the rest waits for the next run", r.cfg.MaxPerRun)
			return finish(true)
		}

		lost, err := r.fetchOne(ctx, c, m)
		if err != nil {
			if !lost && ctx.Err() == nil {
				// Delivery problem: QUIT cleanly so earlier deletes stick.
				finish(true)
			}
			return false, err
		}
		if lost {
			open = false
			c.Close() // no QUIT: the session is wedged; its DELEs are redone next session
			return false, nil
		}
		if !r.cfg.Keep && afterDele() {
			return finish(false)
		}
	}
	return finish(true)
}

// fetchOne downloads, delivers and DELEs one message. lost=true means the
// connection is unusable and the caller must drop it.
func (r *runner) fetchOne(ctx context.Context, c *popClient, m msgInfo) (lost bool, err error) {
	r.st.Retrieving, r.st.RetrievingSize = m.uid, m.size
	if err := r.st.save(); err != nil {
		return false, err
	}
	r.fetched++
	data, rerr := c.retr(m, r.cfg.MessageTimeout.Duration, 2*m.size+1<<20)
	r.st.Retrieving, r.st.RetrievingSize = "", 0

	if rerr != nil {
		if ctx.Err() != nil {
			r.st.save()
			return true, ctx.Err()
		}
		r.failedThisRun[m.uid] = true
		r.recordFailure(m, rerr.Error(), data)
		f := r.st.Failures[m.uid]
		var pe *popError
		if errors.As(rerr, &pe) {
			r.log.Printf("FAILED uid %s (message %d): server refused: %v", m.uid, m.num, rerr)
			return false, r.st.save()
		}
		r.stalls++
		r.log.Printf("FAILED uid %s (message %d, %d bytes, got %d before it died; failure %d/%d): %v %s",
			m.uid, m.num, m.size, len(data), f.Count, r.cfg.PoisonThreshold, rerr, f.Hint)
		return true, r.st.save()
	}

	r.st.Delivering = m.uid
	if err := r.st.save(); err != nil {
		return false, err
	}
	msg := append(receivedHeader(r.cfg.Server, r.host, m.uid, r.cfg.DeliverTo, r.now()), data...)
	dctx, cancel := context.WithTimeout(ctx, r.cfg.DeliverTimeout.Duration)
	derr := r.deliv.Deliver(dctx, msg, envelopeFrom(data))
	cancel()
	r.st.Delivering = ""
	if derr != nil {
		r.st.save()
		return false, fmt.Errorf("delivering uid %s failed, left it on the server: %w", m.uid, derr)
	}
	r.st.Delivered[m.uid] = r.now()
	r.st.forget(m.uid)
	if err := r.st.save(); err != nil {
		return false, err
	}
	r.stats.delivered++
	r.log.Printf("delivered uid %s (%d bytes) %s", m.uid, len(data), headerHint(data))

	if r.cfg.Keep {
		return false, nil
	}
	if err := c.dele(m.num); err != nil {
		r.log.Printf("DELE %d failed after delivery (%v); will retry next session", m.num, err)
		return true, nil
	}
	return false, nil
}

func (r *runner) recordFailure(m msgInfo, reason string, partial []byte) {
	f := r.st.Failures[m.uid]
	if f == nil {
		f = &Failure{}
		r.st.Failures[m.uid] = f
	}
	f.Count++
	f.LastTry = r.now()
	f.LastError = reason
	if m.size > 0 {
		f.Size = m.size
	}
	if h := headerHint(partial); h != "" {
		f.Hint = h
	}
	if len(partial) > 0 && r.cfg.QuarantineDir != "" {
		if err := os.MkdirAll(r.cfg.QuarantineDir, 0o700); err == nil {
			p := filepath.Join(r.cfg.QuarantineDir, sanitizeToken(m.uid)+".partial")
			if err := os.WriteFile(p, partial, 0o600); err == nil {
				f.Partial = p
			}
		}
	}
	r.stats.failures++
}
