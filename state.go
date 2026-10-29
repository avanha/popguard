package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type Failure struct {
	Count     int       `json:"count"`
	LastTry   time.Time `json:"last_try"`
	LastError string    `json:"last_error"`
	Size      int64     `json:"size"`
	Hint      string    `json:"hint,omitempty"`    // From/Subject salvaged from the partial download
	Partial   string    `json:"partial,omitempty"` // quarantine file with the bytes that did arrive
}

// State is the per-account memory that makes runs safe to kill at any point.
type State struct {
	// Set before RETR, cleared after. Still set at startup = a run died
	// (e.g. SIGKILL) mid-download, which counts as a failure for that uid.
	Retrieving     string `json:"retrieving,omitempty"`
	RetrievingSize int64  `json:"retrieving_size,omitempty"`
	// Set before handing a message to the MTA, cleared after.
	Delivering string `json:"delivering,omitempty"`
	// Messages handed to the MTA. If one is still on the server later, its
	// DELE never committed, so we delete it again without redelivering.
	Delivered map[string]time.Time `json:"delivered"`
	Failures  map[string]*Failure  `json:"failures"`

	path string
}

func loadState(path string) (*State, error) {
	st := &State{path: path}
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal(b, st); err != nil {
			return nil, fmt.Errorf("state file %s is corrupt (fix or move it aside): %w", path, err)
		}
	}
	if st.Delivered == nil {
		st.Delivered = map[string]time.Time{}
	}
	if st.Failures == nil {
		st.Failures = map[string]*Failure{}
	}
	return st, nil
}

// forget drops a message's failure record and its quarantined partial download.
func (s *State) forget(uid string) {
	if f := s.Failures[uid]; f != nil && f.Partial != "" {
		os.Remove(f.Partial)
	}
	delete(s.Failures, uid)
}

// save writes atomically: a crash leaves either the old or the new file.
func (s *State) save() error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

var errLocked = errors.New("locked by another run")

// lockState keeps overlapping cron runs from fighting over the mailbox.
func lockState(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errLocked
		}
		return nil, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
