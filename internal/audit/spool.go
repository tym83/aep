package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

var ErrFull = errors.New("audit spool capacity exceeded")

type state struct {
	Stream   string            `json:"stream"`
	Sequence uint64            `json:"sequence"`
	Head     string            `json:"head"`
	Pending  []Event           `json:"pending"`
	Seen     map[string]string `json:"seen"`
}
type Spool struct {
	mu    sync.Mutex
	dir   string
	max   int
	state state
	lock  *os.File
}

func Open(dir, stream string, maxBytes int) (*Spool, error) {
	if stream == "" || maxBytes < 1024 {
		return nil, fmt.Errorf("stream and capacity required")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	l, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(l.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		l.Close()
		return nil, err
	}
	s := &Spool{dir: dir, max: maxBytes, lock: l, state: state{Stream: stream, Seen: map[string]string{}}}
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err == nil {
		decoder := json.NewDecoder(bytes.NewReader(b))
		decoder.UseNumber()
		err = decoder.Decode(&s.state)
	} else if os.IsNotExist(err) {
		err = nil
	}
	if err == nil && s.state.Stream != stream {
		err = fmt.Errorf("spool stream mismatch")
	}
	if err == nil && len(s.state.Pending) > 0 {
		p := s.state.Pending
		_, err = Verify(bytes.NewReader(JSONL(p)), p[0].PrevHash, s.state.Head)
		if err == nil && p[len(p)-1].Sequence != s.state.Sequence {
			err = fmt.Errorf("spool sequence mismatch")
		}
	}
	if err != nil {
		s.Close()
		return nil, err
	}
	if s.state.Seen == nil {
		s.state.Seen = map[string]string{}
	}
	return s, nil
}
func (s *Spool) Close() error { return s.lock.Close() }
func (s *Spool) save(st state) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if len(b) > s.max {
		return ErrFull
	}
	f, err := os.OpenFile(filepath.Join(s.dir, "state.tmp"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err != nil {
		return err
	}
	if ce != nil {
		return ce
	}
	if err = os.Rename(f.Name(), filepath.Join(s.dir, "state.json")); err != nil {
		return err
	}
	d, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	d.Close()
	// A directory fsync failure is fatal: the caller must restart, not reuse old state.
	s.state = st
	return err
}
func (s *Spool) Append(events []Event) error { return s.Observe("", "", events) }
func (s *Spool) Seen(key string) string      { s.mu.Lock(); defer s.mu.Unlock(); return s.state.Seen[key] }
func (s *Spool) Observe(key, revision string, events []Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if key != "" && s.state.Seen[key] == revision {
		return nil
	}
	st := s.state
	st.Pending = append([]Event(nil), st.Pending...)
	st.Seen = map[string]string{}
	for k, v := range s.state.Seen {
		st.Seen[k] = v
	}
	for _, e := range events {
		st.Sequence++
		e.seal(st.Stream, st.Sequence, st.Head)
		st.Head = e.Hash
		st.Pending = append(st.Pending, e)
	}
	if key != "" {
		st.Seen[key] = revision
	}
	return s.save(st)
}
func (s *Spool) Pending() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.state.Pending...)
}
func (s *Spool) Ack(last uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state
	i := 0
	for i < len(st.Pending) && st.Pending[i].Sequence <= last {
		i++
	}
	st.Pending = st.Pending[i:]
	return s.save(st)
}

type Sink interface {
	Send(context.Context, []Event) error
}

// Flush replays the whole batch after any sink failure. Consumers deduplicate ID.
func (s *Spool) Flush(ctx context.Context, sinks []Sink) error {
	if len(sinks) == 0 {
		return fmt.Errorf("at least one sink required")
	}
	p := s.Pending()
	if len(p) == 0 {
		return nil
	}
	if len(p) > 100 {
		p = p[:100]
	}
	for _, sink := range sinks {
		if err := sink.Send(ctx, p); err != nil {
			return err
		}
	}
	return s.Ack(p[len(p)-1].Sequence)
}
