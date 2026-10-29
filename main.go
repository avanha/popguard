// popguard: a defensive POP3 fetcher. It downloads mail and hands it to an
// MTA (SMTP) or MDA like fetchmail, but assumes the server is flaky:
//
//   - every read has an idle timeout and every message a hard time limit,
//     so a server that goes silent mid-message can't wedge a run;
//   - a message that kills the connection is recorded by UIDL, and after
//     poison_threshold failures it is skipped (or deleted) on every later
//     run, so one bad message can't block the rest of the mailbox;
//   - deletes are committed in small batches (QUIT/reconnect), and every
//     delivered UIDL is remembered, so an aborted session never causes
//     duplicate deliveries;
//   - state is saved before and after each risky step, so the process can
//     be killed at any point and the next run picks up correctly.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"text/tabwriter"
	"time"
)

func main() { os.Exit(realMain()) }

type options struct {
	verbose, list, status bool
	reset                 string
	resetHit              bool // some account had the uid named by -reset
}

func realMain() int {
	cfgArg := flag.String("config", "", "config file, or a directory of *.json account files (default: ~/.config/popguard/conf.d)")
	var o options
	flag.BoolVar(&o.verbose, "v", false, "log POP3 commands and replies (password redacted)")
	flag.BoolVar(&o.list, "list", false, "show messages on the server and their local status; downloads nothing")
	flag.BoolVar(&o.status, "status", false, "show local state (failures, poison) without connecting")
	flag.StringVar(&o.reset, "reset", "", `forget the failure history of a UIDL ("all" for every one) so it is retried`)
	flag.Parse()

	paths, err := configPaths(*cfgArg)
	if err != nil {
		log.New(os.Stderr, "popguard: ", log.LstdFlags).Printf("config: %v", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Like fetchmail, one bad account doesn't stop the others; the exit
	// status reports the worst result.
	worst := 0
	for _, p := range paths {
		if ctx.Err() != nil {
			break
		}
		if len(paths) > 1 && (o.list || o.status) {
			fmt.Printf("== %s ==\n", accountName(p))
		}
		worst = max(worst, runAccount(ctx, p, &o))
	}
	if o.reset != "" && o.reset != "all" && !o.resetHit && worst == 0 {
		fmt.Fprintf(os.Stderr, "popguard: no failure record for uid %q\n", o.reset)
		return 1
	}
	return worst
}

func runAccount(ctx context.Context, path string, o *options) int {
	logger := log.New(os.Stderr, "popguard: "+accountName(path)+": ", log.LstdFlags)
	cfg, err := loadConfig(path)
	if err != nil {
		logger.Printf("config: %v", err)
		return 2
	}
	for _, w := range cfg.warnings {
		logger.Printf("warning: %s", w)
	}

	unlock, err := lockState(cfg.StateFile)
	if errors.Is(err, errLocked) {
		logger.Printf("another run is still going (%s.lock); skipping this one", cfg.StateFile)
		return 0
	}
	if err != nil {
		logger.Printf("lock: %v", err)
		return 1
	}
	defer unlock()

	st, err := loadState(cfg.StateFile)
	if err != nil {
		logger.Print(err)
		return 1
	}

	switch {
	case o.reset != "":
		if o.reset == "all" {
			for u := range st.Failures {
				st.forget(u)
			}
		} else if _, ok := st.Failures[o.reset]; !ok {
			return 0 // realMain complains if no account had it
		} else {
			st.forget(o.reset)
		}
		o.resetHit = true
		if err := st.save(); err != nil {
			logger.Print(err)
			return 1
		}
		fmt.Println("reset", o.reset)
		return 0

	case o.status:
		printStatus(os.Stdout, cfg, st)
		return 0

	case o.list:
		if err := listMessages(ctx, newRunner(cfg, st, logger, o.verbose), os.Stdout); err != nil {
			logger.Print(err)
			return 1
		}
		return 0
	}

	r := newRunner(cfg, st, logger, o.verbose)
	err = r.run(ctx)
	s := r.stats
	if s.delivered+s.redeleted+s.poisonSkipped+s.poisonDeleted+s.failures > 0 {
		logger.Printf("summary: %d delivered, %d re-deleted, %d poison skipped, %d poison deleted, %d failures",
			s.delivered, s.redeleted, s.poisonSkipped, s.poisonDeleted, s.failures)
	}
	if err != nil {
		logger.Printf("error: %v", err)
		return 1
	}
	return 0
}

func listMessages(ctx context.Context, r *runner, out io.Writer) error {
	c, err := r.connect(ctx)
	if err != nil {
		return err
	}
	msgs, err := c.inventory()
	if err != nil {
		c.Close()
		return err
	}
	c.quit()

	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NUM\tUID\tSIZE\tSTATUS\tDETAIL")
	for _, m := range msgs {
		status, detail := "new", ""
		if _, ok := r.st.Delivered[m.uid]; ok {
			status = "delivered, delete pending"
			if r.cfg.Keep {
				status = "delivered (kept)"
			}
		} else if f := r.st.Failures[m.uid]; f != nil {
			status = fmt.Sprintf("failed %dx", f.Count)
			if f.Count >= r.cfg.PoisonThreshold {
				status = fmt.Sprintf("POISON (%dx)", f.Count)
			}
			detail = f.Hint
			if detail == "" {
				detail = f.LastError
			}
		}
		fmt.Fprintf(tw, "%d\t%s\t%d\t%s\t%s\n", m.num, m.uid, m.size, status, truncate(detail, 80))
	}
	tw.Flush()
	fmt.Fprintf(out, "%d messages\n", len(msgs))
	return nil
}

func printStatus(out io.Writer, cfg *Config, st *State) {
	fmt.Fprintf(out, "state file: %s\n", cfg.StateFile)
	fmt.Fprintf(out, "remembered deliveries: %d\n", len(st.Delivered))
	if st.Retrieving != "" {
		fmt.Fprintf(out, "interrupted download: %s\n", st.Retrieving)
	}
	if len(st.Failures) == 0 {
		fmt.Fprintln(out, "no failing messages")
		return
	}
	uids := make([]string, 0, len(st.Failures))
	for u := range st.Failures {
		uids = append(uids, u)
	}
	sort.Strings(uids)
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "UID\tFAILS\tLAST TRY\tSIZE\tDETAIL")
	for _, u := range uids {
		f := st.Failures[u]
		tag := ""
		if f.Count >= cfg.PoisonThreshold {
			tag = " POISON"
		}
		fmt.Fprintf(tw, "%s\t%d%s\t%s\t%d\t%s\n", u, f.Count, tag, f.LastTry.Format(time.DateTime), f.Size, truncate(f.Hint, 70))
		if f.Partial != "" {
			fmt.Fprintf(tw, "\t\t\t\tpartial: %s\n", f.Partial)
		}
	}
	tw.Flush()
}
