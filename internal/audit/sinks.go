package audit

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

type OpenSearch struct {
	URL, Index, Username, Password string
	Client                         *http.Client
	DeadLetters                    ObjectWriter
}

func (s OpenSearch) Send(ctx context.Context, events []Event) error {
	if !strings.HasPrefix(s.URL, "https://") && !strings.HasPrefix(s.URL, "http://127.0.0.1:") {
		return fmt.Errorf("OpenSearch requires HTTPS")
	}
	var b bytes.Buffer
	for _, e := range events {
		m, _ := json.Marshal(map[string]any{"index": map[string]string{"_index": s.Index, "_id": e.Hash}})
		b.Write(m)
		b.WriteByte('\n')
		raw, _ := json.Marshal(e.Details)
		// Preserve the sealed event while indexing details without dynamic field types.
		document := struct {
			Event
			DetailsRaw string `json:"detailsRaw"`
		}{e, string(raw)}
		encoded, _ := json.Marshal(document)
		b.Write(encoded)
		b.WriteByte('\n')
	}
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(s.URL, "/")+"/_bulk", &b)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	if s.Username != "" {
		req.SetBasicAuth(s.Username, s.Password)
	}
	c := s.Client
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("OpenSearch HTTP %d", resp.StatusCode)
	}
	var result struct {
		Errors bool `json:"errors"`
		Items  []map[string]struct {
			Status int `json:"status"`
			Error  struct {
				Type   string `json:"type"`
				Reason string `json:"reason"`
			} `json:"error"`
		} `json:"items"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&result); err != nil {
		return err
	}
	if len(result.Items) != len(events) {
		return fmt.Errorf("OpenSearch incomplete or failed bulk")
	}
	var failures []error
	for i, item := range result.Items {
		v, ok := item["index"]
		if !ok {
			failures = append(failures, fmt.Errorf("OpenSearch missing index result"))
			continue
		}
		if v.Status/100 == 2 {
			continue
		}
		if v.Status >= 400 && v.Status < 500 && v.Status != 408 && v.Status != 429 && v.Status != 401 && v.Status != 403 {
			if err := deadLetter(ctx, s.DeadLetters, "opensearch", events[i], v.Error.Type, v.Error.Reason); err != nil {
				failures = append(failures, err)
			}
		} else {
			failures = append(failures, fmt.Errorf("OpenSearch retryable item status=%d error.type=%q error.reason=%q", v.Status, v.Error.Type, v.Error.Reason))
		}
	}
	return errors.Join(failures...)
}

type Syslog struct {
	Address string
	TLS     *tls.Config
}

func (s Syslog) Send(ctx context.Context, events []Event) error {
	cfg := s.TLS
	if cfg == nil {
		cfg = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: cfg}
	c, e := d.DialContext(ctx, "tcp", s.Address)
	if e != nil {
		return e
	}
	defer c.Close()
	if e = c.SetDeadline(time.Now().Add(30 * time.Second)); e != nil {
		return e
	}
	for _, v := range events {
		m := syslogMessage(v)
		if _, e = fmt.Fprintf(c, "%d %s", len(m), m); e != nil {
			return e
		}
	}
	return nil
}

// RFC5424 permits at most six fractional digits in the header timestamp.
// Preserve the original precision in the JSON message and thus its chain hash.
func syslogMessage(e Event) string {
	timestamp := "-"
	if t, err := time.Parse(time.RFC3339Nano, e.Time); err == nil {
		timestamp = t.UTC().Format("2006-01-02T15:04:05.000000Z")
	}
	b, _ := json.Marshal(e)
	return fmt.Sprintf("<134>1 %s - aep-audit - event - %s", timestamp, b)
}

type ObjectWriter interface {
	Put(context.Context, string, []byte, string) error
}
type S3 struct{ Store ObjectWriter }

func (s S3) Send(ctx context.Context, events []Event) error {
	// Immutable chunks in UTC daily partitions; never rewrite a daily object.
	for start := 0; start < len(events); {
		end := start + 1
		stamp, err := time.Parse(time.RFC3339Nano, events[start].RecordedAt)
		if err != nil {
			return err
		}
		day := stamp.UTC().Format("2006-01-02")
		for end < len(events) && strings.HasPrefix(events[end].RecordedAt, day) {
			end++
		}
		p := events[start:end]
		key := fmt.Sprintf("%s/%s/%020d-%020d-%s.jsonl", events[start].Stream, day, p[0].Sequence, p[len(p)-1].Sequence, p[len(p)-1].Hash)
		if e := s.Store.Put(ctx, key, JSONL(p), "application/x-ndjson"); e != nil {
			return e
		}
		start = end
	}
	return nil
}
