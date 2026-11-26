// Package audit normalizes observations into a versioned, tamper-evident stream.
package audit

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

const Version = "aep.audit/v1"

type Event struct {
	RecordedAt   string         `json:"recordedAt"`
	Version      string         `json:"version"`
	Stream       string         `json:"stream"`
	Sequence     uint64         `json:"sequence"`
	ID           string         `json:"id"`
	Time         string         `json:"time"`
	Source       string         `json:"source"`
	Action       string         `json:"action"`
	Actor        string         `json:"actor"`
	Organization string         `json:"organization,omitempty"`
	Resource     string         `json:"resource"`
	Details      map[string]any `json:"details,omitempty"`
	PrevHash     string         `json:"prevHash"`
	Hash         string         `json:"hash"`
}

func Digest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (e Event) digest() string { e.Hash = ""; return Digest(e) }
func (e *Event) seal(stream string, seq uint64, prev string) {
	e.RecordedAt = time.Now().UTC().Format(time.RFC3339Nano)
	e.Version = Version
	e.Stream = stream
	e.Sequence = seq
	e.PrevHash = prev
	if e.Time == "" {
		e.Time = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if e.ID == "" {
		e.ID = Digest([]any{stream, seq, e.Source, e.Resource, e.Action})
	}
	e.Hash = e.digest()
}

// Verify requires an externally trusted predecessor and optionally a trusted
// final hash. An empty predecessor means genesis. It never silently trusts the
// first record's predecessor. Hashes prove continuity, not writer authenticity.
func Verify(r io.Reader, prev, final string) (string, error) {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 4096), 16<<20)
	var seq uint64
	var stream string
	n := 0
	for s.Scan() {
		var e Event
		decoder := json.NewDecoder(bytes.NewReader(s.Bytes()))
		decoder.UseNumber()
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&e); err != nil {
			return prev, err
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			return prev, fmt.Errorf("trailing JSON at record %d", n+1)
		}
		if e.Stream == "" || e.Sequence == 0 || e.Version != Version || e.Hash != e.digest() || e.PrevHash != prev || (n > 0 && (e.Sequence != seq+1 || e.Stream != stream)) || (n == 0 && prev == "" && e.Sequence != 1) {
			return prev, fmt.Errorf("invalid chain at record %d", n+1)
		}
		prev = e.Hash
		seq = e.Sequence
		stream = e.Stream
		n++
	}
	if err := s.Err(); err != nil {
		return prev, err
	}
	if n == 0 {
		return prev, fmt.Errorf("empty export")
	}
	if final != "" && final != prev {
		return prev, fmt.Errorf("final hash mismatch")
	}
	return prev, nil
}
func JSONL(events []Event) []byte {
	var b []byte
	for _, e := range events {
		v, _ := json.Marshal(e)
		b = append(b, v...)
		b = append(b, '\n')
	}
	return b
}
