package itsm

import (
	"context"
	api "github.com/tym83/aep/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"testing"
	"time"
)

func TestPermanentFailureAndReplacement(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 408, 409, 422, 429, 503} {
		t.Run(string(rune(status)), func(t *testing.T) {
			ctx := context.Background()
			now := time.Now()
			calls := 0
			engine := &Engine{Store: testStore(t), Now: func() time.Time { return now }, Adapter: deliveryFunc(func(_ context.Context, e Event, ref string) (string, error) {
				calls++
				if calls == 1 {
					return "saved", &HTTPError{status}
				}
				if ref != "saved" {
					t.Error("reference lost")
				}
				return ref, nil
			})}
			first := changeEvent(api.PhasePending)
			if engine.Submit(ctx, first) == nil {
				t.Fatal("missing failure")
			}
			_, r, _ := engine.Store.get(ctx, first.Subject)
			terminal := status >= 400 && status < 500 && status != 408 && status != 429
			if r.Done != terminal || (r.Failure != 0) != terminal {
				t.Fatal(r)
			}
			second := changeEvent(api.PhaseApplied)
			second.Time = first.Time.Add(time.Second)
			if err := engine.Submit(ctx, second); err != nil {
				t.Fatal(err)
			}
			_, r, _ = engine.Store.get(ctx, first.Subject)
			if r.Event.ID != second.ID || !r.Done {
				t.Fatal(r)
			}
		})
	}
}
func TestIndependentSubjectsAndCleanup(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	entered := make(chan struct{})
	release := make(chan struct{})
	engine := &Engine{Store: testStore(t), Now: func() time.Time { return now }, Adapter: deliveryFunc(func(_ context.Context, e Event, r string) (string, error) {
		if e.Subject == "slow" {
			close(entered)
			<-release
		}
		return r, nil
	})}
	first := NewEvent("change.note", "slow", Note{})
	done := make(chan error, 1)
	go func() { done <- engine.Submit(ctx, first) }()
	<-entered
	fast := NewEvent("change.note", "fast", Note{})
	fastDone := make(chan error, 1)
	go func() { fastDone <- engine.Submit(ctx, fast) }()
	select {
	case err := <-fastDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("global lock held during delivery")
	}
	close(release)
	<-done
	now = now.Add(8 * 24 * time.Hour)
	bad := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "corrupt", Namespace: engine.Store.Namespace, Labels: map[string]string{"aep.aenix.io/itsm-state": "true", "aep.aenix.io/itsm-scope": Digest(nil)[:32]}}, Data: map[string]string{"state": "{"}}
	if err := engine.Store.Client.Create(ctx, bad); err != nil {
		t.Fatal(err)
	}
	bad.Name = "invalid-envelope"
	bad.ResourceVersion = ""
	bad.Data["state"] = "{}"
	if err := engine.Store.Client.Create(ctx, bad); err != nil {
		t.Fatal(err)
	}
	if engine.Retry(ctx) == nil {
		t.Fatal("corruption not reported")
	}
	cm, _, err := engine.Store.get(ctx, "fast")
	if err != nil || cm.ResourceVersion != "" {
		t.Fatal("completed record not cleaned", err)
	}
}
func TestAlertsUseEventTime(t *testing.T) {
	ctx := context.Background()
	calls := 0
	e := &Engine{Store: testStore(t), Adapter: deliveryFunc(func(context.Context, Event, string) (string, error) { calls++; return "", nil })}
	resolved := NewEvent("alert.resolved", "alert/db", Alert{Key: "db"})
	raised := NewEvent("alert.raised", "alert/db", Alert{Key: "db"})
	raised.Time = resolved.Time.Add(-time.Minute)
	if err := e.Submit(ctx, resolved); err != nil {
		t.Fatal(err)
	}
	if err := e.Submit(ctx, raised); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("stale alert reopened")
	}
}
