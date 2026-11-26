package audit

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// OpenChoreoRecord maps one structured OpenChoreo audit record (the JSON
// emitted with msg "AUDIT-LOG", or the same record posted to /openchoreo) to an
// event. Records without an id, action or result are rejected.
func OpenChoreoRecord(source string, b []byte) (Event, bool) {
	var e struct {
		ID       string         `json:"event_id"`
		Time     string         `json:"event_time"`
		Action   string         `json:"action"`
		Result   string         `json:"result"`
		Actor    map[string]any `json:"actor"`
		Resource map[string]any `json:"resource"`
		Request  string         `json:"request_id"`
	}
	if json.Unmarshal(b, &e) != nil || e.ID == "" || e.Action == "" || e.Result == "" {
		return Event{}, false
	}
	if e.Time != "" {
		if _, err := time.Parse(time.RFC3339Nano, e.Time); err != nil {
			return Event{}, false
		}
	}
	actor, _ := e.Actor["id"].(string)
	ns, _ := e.Resource["namespace"].(string)
	return Event{ID: Digest([]string{source, e.ID}), Source: source, Time: e.Time, Action: e.Action, Actor: actor, Organization: ns, Resource: fmt.Sprint(e.Resource["type"], "/", e.Resource["name"]), Details: map[string]any{"actor": e.Actor, "resource": e.Resource, "result": e.Result, "requestID": e.Request}}, true
}

// auditLine returns the record carried by an OpenChoreo log line, if any.
// OpenChoreo routes audit records by the stable message "AUDIT-LOG".
func auditLine(line []byte) ([]byte, bool) {
	var head struct {
		Msg string `json:"msg"`
	}
	if json.Unmarshal(line, &head) != nil || head.Msg != "AUDIT-LOG" {
		return nil, false
	}
	return line, true
}

// recent remembers delivered event IDs, so reconnect overlaps do not append
// the same record twice. Duplicates across restarts remain possible and are
// deduplicated by consumers on the stable event ID.
type recent struct {
	mu    sync.Mutex
	order []string
	seen  map[string]bool
	limit int
}

func (r *recent) add(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen[id] {
		return false
	}
	r.seen[id] = true
	r.order = append(r.order, id)
	if len(r.order) > r.limit {
		delete(r.seen, r.order[0])
		r.order = r.order[1:]
	}
	return true
}

// FollowOpenChoreoAudit collects OpenChoreo API audit records from the
// container logs of the selected pods and appends them to the spool.
//
// OpenChoreo writes its audit records to stdout; this is the log shipper the
// /openchoreo receiver otherwise needs. Each newly seen pod is read from
// lookback before now, reconnects resume from the last record time, and a
// bounded ID set drops overlap duplicates. Log rotation or pod loss before a
// record is read is not recoverable here; alert on the source's own gaps.
func FollowOpenChoreoAudit(ctx context.Context, c kubernetes.Interface, namespace, selector, container, source string, lookback time.Duration, s *Spool) {
	ids := &recent{seen: map[string]bool{}, limit: 10000}
	var mu sync.Mutex
	following := map[string]bool{}
	for ctx.Err() == nil {
		pods, err := c.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			log.Printf("openchoreo audit: list pods: %v", err)
		} else {
			for _, p := range pods.Items {
				mu.Lock()
				started := following[p.Name]
				if !started && p.Status.Phase == corev1.PodRunning {
					following[p.Name] = true
				}
				mu.Unlock()
				if started || p.Status.Phase != corev1.PodRunning {
					continue
				}
				go func(pod string) {
					followPod(ctx, c, namespace, pod, container, source, time.Now().Add(-lookback), ids, s)
					mu.Lock()
					delete(following, pod)
					mu.Unlock()
				}(p.Name)
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(30 * time.Second):
		}
	}
}

func followPod(ctx context.Context, c kubernetes.Interface, namespace, pod, container, source string, since time.Time, ids *recent, s *Spool) {
	for ctx.Err() == nil {
		t := metav1.NewTime(since)
		stream, err := c.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{Container: container, Follow: true, SinceTime: &t}).Stream(ctx)
		if err != nil {
			// The pod is gone or not readable; the pod loop picks up replacements.
			log.Printf("openchoreo audit: logs of %s: %v", pod, err)
			return
		}
		scanner := bufio.NewScanner(stream)
		scanner.Buffer(make([]byte, 64<<10), 1<<20)
		for scanner.Scan() {
			record, ok := auditLine(scanner.Bytes())
			if !ok {
				continue
			}
			e, ok := OpenChoreoRecord(source, record)
			if !ok {
				InvalidWebhookCount.Add(1)
				continue
			}
			if !ids.add(e.ID) {
				continue
			}
			for ctx.Err() == nil {
				if err := s.Append([]Event{e}); err == nil {
					break
				} else {
					log.Printf("openchoreo audit: spool: %v", err)
				}
				time.Sleep(5 * time.Second)
			}
			if ts, err := time.Parse(time.RFC3339Nano, e.Time); err == nil && ts.After(since) {
				since = ts
			}
		}
		stream.Close()
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
}
