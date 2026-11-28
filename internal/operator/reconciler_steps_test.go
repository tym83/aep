package operator

import (
	"context"
	"errors"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fakeStatus is a non-Organization subject of the generic step runner.
type fakeStatus struct {
	types   []string
	conds   []metav1.Condition
	gen     int64
	phase   string
	ready   string
	updates int
}

func (s *fakeStatus) conditionTypes() []string       { return s.types }
func (s *fakeStatus) conditions() []metav1.Condition { return s.conds }
func (s *fakeStatus) generation() int64              { return s.gen }
func (s *fakeStatus) setPhase(p string)              { s.phase = p }
func (s *fakeStatus) readyPhase() string             { return s.ready }
func (s *fakeStatus) updateStatus(context.Context) error {
	s.updates++
	return nil
}
func (s *fakeStatus) setCondition(t string, ok bool, reason, msg string) {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&s.conds, metav1.Condition{Type: t, Status: status, ObservedGeneration: s.gen, Reason: reason, Message: msg})
}

func reason(s *fakeStatus, t string) string {
	if c := meta.FindStatusCondition(s.conds, t); c != nil {
		return string(c.Status) + "/" + c.Reason
	}
	return ""
}

func TestRunReconcileStepsGeneric(t *testing.T) {
	ok := func(context.Context) error { return nil }
	t.Run("all succeed", func(t *testing.T) {
		st := &fakeStatus{types: []string{"A", "B", "Ready"}, gen: 3, ready: "Ready"}
		res, err := runReconcileSteps(context.Background(), st, []reconcileStep{{name: "A", run: ok}, {name: "B", run: ok, message: func() string { return "custom" }}})
		if err != nil || res.RequeueAfter != time.Minute || st.phase != "Ready" || st.updates != 1 {
			t.Fatal(res, err, st)
		}
		if reason(st, "A") != "True/Reconciled" || meta.FindStatusCondition(st.conds, "B").Message != "custom" || reason(st, "Ready") != "False/Pending" {
			t.Fatal(st.conds)
		}
	})
	t.Run("blocking pending", func(t *testing.T) {
		st := &fakeStatus{types: []string{"A", "B"}, gen: 1, ready: "Ready"}
		ran := false
		res, err := runReconcileSteps(context.Background(), st, []reconcileStep{
			{name: "A", run: func(context.Context) error { return pending("waiting") }},
			{name: "B", run: func(context.Context) error { ran = true; return nil }},
		})
		if err != nil || res.RequeueAfter != 30*time.Second || ran || st.phase != "Pending" {
			t.Fatal(res, err, ran, st.phase)
		}
		if reason(st, "A") != "False/Pending" || reason(st, "B") != "False/DependencyPending" {
			t.Fatal(st.conds)
		}
	})
	t.Run("blocking error", func(t *testing.T) {
		st := &fakeStatus{types: []string{"A"}, ready: "Ready"}
		_, err := runReconcileSteps(context.Background(), st, []reconcileStep{{name: "A", run: func(context.Context) error { return errors.New("boom") }}})
		if err == nil || err.Error() != "A: boom" || reason(st, "A") != "False/Retrying" {
			t.Fatal(err, st.conds)
		}
	})
	t.Run("non-blocking and held", func(t *testing.T) {
		st := &fakeStatus{types: []string{"A", "B", "C"}, ready: "Ready"}
		res, err := runReconcileSteps(context.Background(), st, []reconcileStep{
			{name: "A", run: func(context.Context) error { return errors.New("soft") }, nonBlocking: true},
			{name: "B", run: func(context.Context) error { return errors.New("policy") }, nonBlocking: true, holdsReady: true, reason: func(error) string { return "Custom" }},
			{name: "C", run: ok},
		})
		if err != nil || res.RequeueAfter != time.Minute || st.phase != "Pending" {
			t.Fatal(res, err, st.phase)
		}
		if reason(st, "A") != "False/Retrying" || reason(st, "B") != "False/Custom" || reason(st, "C") != "True/Reconciled" {
			t.Fatal(st.conds)
		}
	})
	t.Run("ready gate", func(t *testing.T) {
		st := &fakeStatus{ready: "Pending"}
		if _, err := runReconcileSteps(context.Background(), st, []reconcileStep{{name: "A", run: ok, partial: true}}); err != nil || st.phase != "Pending" || len(st.conds) != 0 {
			t.Fatal(err, st)
		}
	})
}
