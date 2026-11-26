package itsm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func Digest(raw []byte) string { v := sha256.Sum256(raw); return hex.EncodeToString(v[:]) }

type Receipt struct {
	Hash  string    `json:"hash"`
	Until time.Time `json:"until"`
}
type Record struct {
	Receipts map[string]Receipt `json:"receipts,omitempty"`
	Event    Event              `json:"event"`
	Ref      string             `json:"ref,omitempty"`
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

// Engine serializes deliveries and commits the durable cursor only after success.
// A single active process is required; the chart uses Recreate and one replica.
type Engine struct {
	Store   Store
	Adapter Adapter
	mu      sync.Mutex
	Now     func() time.Time
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
	e.mu.Lock()
	defer e.mu.Unlock()
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
		return nil
	}
	if len(r.Receipts) >= 2000 {
		return errors.New("receipt capacity reached; wait for seven-day expiry")
	}
	if r.Event.ID == event.ID {
		if string(r.Event.Data) != string(event.Data) || r.Event.Type != event.Type {
			return errors.New("event ID reused with different payload")
		}
		if r.Done {
			return nil
		}
	} else {
		if r.Event.ID != "" && !r.Done {
			return ErrBusy
		}
		r = Record{Event: event, Ref: r.Ref, Receipts: r.Receipts}
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
	if err == nil {
		r.Done = true
		if r.Receipts == nil {
			r.Receipts = map[string]Receipt{}
		}
		r.Receipts[r.Event.ID] = Receipt{Hash: Digest(append([]byte(r.Event.Type), r.Event.Data...)), Until: e.now().Add(7 * 24 * time.Hour)}
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
	e.mu.Lock()
	defer e.mu.Unlock()
	list := &corev1.ConfigMapList{}
	if err := e.Store.Client.List(ctx, list, client.InNamespace(e.Store.Namespace), client.MatchingLabels{"aep.aenix.io/itsm-state": "true", "aep.aenix.io/itsm-scope": Digest([]byte(e.Store.Scope))[:32]}); err != nil {
		return err
	}
	var errs []error
	for i := range list.Items {
		cm := &list.Items[i]
		var r Record
		if err := json.Unmarshal([]byte(cm.Data["state"]), &r); err != nil {
			return err
		}
		if !r.Done && !e.now().Before(r.Next) {
			errs = append(errs, e.deliver(ctx, cm, r))
		}
	}
	return errors.Join(errs...)
}
