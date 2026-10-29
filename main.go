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

func realMain() int {
	cfgPath := flag.String("config", defaultConfigPath(), "config file")
	verbose := flag.Bool("v", false, "log POP3 commands and replies (password redacted)")
	list := flag.Bool("list", false, "show messages on the server and their local status; downloads nothing")
	status := flag.Bool("status", false, "show local state (failures, poison) without connecting")
	reset := flag.String("reset", "", `forget the failure history of a UIDL ("all" for every one) so it is retried`)
	flag.Parse()

	logger := log.New(os.Stderr, "popguard: ", log.LstdFlags)
	cfg, err := loadConfig(*cfgPath)
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch {
	case *reset != "":
		if *reset == "all" {
			st.Failures = map[string]*Failure{}
		} else if _, ok := st.Failures[*reset]; !ok {
			logger.Printf("no failure record for uid %q", *reset)
			return 1
		} else {
			delete(st.Failures, *reset)
		}
		if err := st.save(); err != nil {
			logger.Print(err)
			return 1
		}
		fmt.Println("reset", *reset)
		return 0

	case *status:
		printStatus(os.Stdout, cfg, st)
		return 0

	case *list:
		if err := listMessages(ctx, newRunner(cfg, st, logger, *verbose), os.Stdout); err != nil {
			logger.Print(err)
			return 1
		}
		return 0
	}

	r := newRunner(cfg, st, logger, *verbose)
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
