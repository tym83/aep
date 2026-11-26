package itsm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func Digest(raw []byte) string { v := sha256.Sum256(raw); return hex.EncodeToString(v[:]) }

type Receipt struct {
	Status int       `json:"status,omitempty"`
	Hash   string    `json:"hash"`
	Until  time.Time `json:"until"`
}
type Record struct {
	Snapshot json.RawMessage    `json:"snapshot,omitempty"`
	Receipts map[string]Receipt `json:"receipts,omitempty"`
	Event    Event              `json:"event"`
	Ref      string             `json:"ref,omitempty"`
	Failure  int                `json:"failure,omitempty"`
	Done     bool               `json:"done"`
	Attempts int                `json:"attempts"`
	Next     time.Time          `json:"next"`
}
type Store struct {
	Client    client.Client
	Namespace string
	Scope     string
}

func (s Store) get(ctx context.Context, subject string) (*corev1.ConfigMap, Record, error) {
	cm := &corev1.ConfigMap{}
	e := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: "itsm-" + Digest([]byte(s.Scope + "/" + subject))[:48]}, cm)
	if apierrors.IsNotFound(e) {
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: "itsm-" + Digest([]byte(s.Scope + "/" + subject))[:48], Labels: map[string]string{"aep.aenix.io/itsm-state": "true", "aep.aenix.io/itsm-scope": Digest([]byte(s.Scope))[:32]}}}, Record{}, nil
	}
	if e != nil {
		return nil, Record{}, e
	}
	var r Record
	e = json.Unmarshal([]byte(cm.Data["state"]), &r)
	return cm, r, e
}
func (s Store) save(ctx context.Context, cm *corev1.ConfigMap, r Record) error {
	b, e := json.Marshal(r)
	if e != nil {
		return e
	}
	if len(b) > 700000 {
		return errors.New("delivery exceeds ConfigMap size budget")
	}
	cm.Data = map[string]string{"state": string(b)}
	if cm.ResourceVersion == "" {
		return s.Client.Create(ctx, cm)
	}
	return s.Client.Update(ctx, cm)
}

type Adapter interface {
	Deliver(context.Context, Event, string) (string, error)
}

// Engine serializes each subject and records successful or terminal outcomes.
// A single active process is required; the chart uses Recreate and one replica.
type Engine struct {
	Store   Store
	Adapter Adapter
	mu      sync.Mutex
	locks   map[string]*subjectLock
	Now     func() time.Time
}

type subjectLock struct {
	sync.Mutex
	users int
}

func (e *Engine) lock(subject string) func() {
	e.mu.Lock()
	if e.locks == nil {
		e.locks = map[string]*subjectLock{}
	}
	l := e.locks[subject]
	if l == nil {
		l = &subjectLock{}
		e.locks[subject] = l
	}
	l.users++
	e.mu.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		e.mu.Lock()
		defer e.mu.Unlock()
		l.users--
		if l.users == 0 {
			delete(e.locks, subject)
		}
	}
}
func permanent(err error) int {
	var h *HTTPError
	if errors.As(err, &h) && h.Status >= 400 && h.Status < 500 && h.Status != 408 && h.Status != 429 {
		return h.Status
	}
	return 0
}
func outcome(status int) error {
	if status != 0 {
		return &HTTPError{Status: status}
	}
	return nil
}

var ErrBusy = errors.New("previous delivery is pending; retry later")

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}
func (e *Engine) Submit(ctx context.Context, event Event) error {
	if err := event.Validate(); err != nil {
		return err
	}
	unlock := e.lock(event.Subject)
	defer unlock()
	cm, r, err := e.Store.get(ctx, event.Subject)
	if err != nil {
		return err
	}
	if r.Receipts == nil {
		r.Receipts = map[string]Receipt{}
	}
	for id, receipt := range r.Receipts {
		if e.now().After(receipt.Until) {
			delete(r.Receipts, id)
		}
	}
	hash := Digest(append([]byte(event.Type), event.Data...))
	if receipt, ok := r.Receipts[event.ID]; ok {
		if receipt.Hash != hash {
			return errors.New("event ID reused with different payload")
		}
		return outcome(receipt.Status)
	}
	if len(r.Receipts) >= 2000 {
		return errors.New("receipt capacity reached; wait for seven-day expiry")
	}
	if r.Event.ID == event.ID {
		if string(r.Event.Data) != string(event.Data) || r.Event.Type != event.Type {
			return errors.New("event ID reused with different payload")
		}
		if r.Done {
			return outcome(r.Failure)
		}
	} else {
		if r.Event.ID != "" {
			if !event.Time.After(r.Event.Time) && (strings.HasPrefix(event.Subject, "change/") || strings.HasPrefix(event.Subject, "cmdb/") || strings.HasPrefix(event.Subject, "alert/")) {
				return nil
			}
			if !r.Done && !(strings.HasPrefix(event.Subject, "change/") || strings.HasPrefix(event.Subject, "cmdb/") || strings.HasPrefix(event.Subject, "alert/")) {
				return ErrBusy
			}
		}
		r = Record{Event: event, Ref: r.Ref, Receipts: r.Receipts, Snapshot: r.Snapshot}
		if err = e.Store.save(ctx, cm, r); err != nil {
			return err
		}
	}
	return e.deliver(ctx, cm, r)
}
func (e *Engine) deliver(ctx context.Context, cm *corev1.ConfigMap, r Record) error {
	if e.now().Before(r.Next) {
		return ErrBusy
	}
	ref, err := e.Adapter.Deliver(ctx, r.Event, r.Ref)
	if ref != "" {
		r.Ref = ref
	}
	if err == nil && r.Event.Type == "cmdb.sync" {
		r.Snapshot = append(json.RawMessage(nil), r.Event.Data...)
	}
	r.Failure = permanent(err)
	if err == nil || r.Failure != 0 {
		r.Done = true
		if r.Receipts == nil {
			r.Receipts = map[string]Receipt{}
		}
		r.Receipts[r.Event.ID] = Receipt{Status: r.Failure, Hash: Digest(append([]byte(r.Event.Type), r.Event.Data...)), Until: e.now().Add(7 * 24 * time.Hour)}
		r.Attempts = 0
		r.Next = time.Time{}
	} else {
		r.Attempts++
		r.Next = e.now().Add(time.Second * time.Duration(1<<min(r.Attempts, 8)))
	}
	if saveErr := e.Store.save(ctx, cm, r); saveErr != nil {
		return fmt.Errorf("persist delivery outcome: %w", saveErr)
	}
	return err
}
func (e *Engine) Retry(ctx context.Context) error {
	list := &corev1.ConfigMapList{}
	if err := e.Store.Client.List(ctx, list, client.InNamespace(e.Store.Namespace), client.MatchingLabels{"aep.aenix.io/itsm-state": "true", "aep.aenix.io/itsm-scope": Digest([]byte(e.Store.Scope))[:32]}); err != nil {
		return err
	}
	var errs []error
	for i := range list.Items {
		cm := &list.Items[i]
		var r Record
		if err := json.Unmarshal([]byte(cm.Data["state"]), &r); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := r.Event.Validate(); err != nil {
			errs = append(errs, err)
			continue
		}
		unlock := e.lock(r.Event.Subject)
		cm, r, err := e.Store.get(ctx, r.Event.Subject)
		if err != nil {
			unlock()
			errs = append(errs, err)
			continue
		}
		if r.Done {
			expired := true
			for _, receipt := range r.Receipts {
				if e.now().Before(receipt.Until) {
					expired = false
				}
			}
			if expired {
				errs = append(errs, e.Store.Client.Delete(ctx, cm))
			}
		}
		if !r.Done && !e.now().Before(r.Next) {
			errs = append(errs, e.deliver(ctx, cm, r))
		}
		unlock()
	}
	return errors.Join(errs...)
}
