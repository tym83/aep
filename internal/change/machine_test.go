package change

import (
	"testing"
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDeadlineAndExpiry(t *testing.T) {
	created := testNow.Add(-time.Hour)
	cr := changeRequest("a", promoteSpec(api.ModelNormal, "v1"), api.PhasePending, created)
	if got := Deadline(&cr, DefaultApprovalTTL); !got.Equal(created.Add(72 * time.Hour)) {
		t.Fatalf("expected creation+72h, got %v", got)
	}
	if Expired(&cr, testNow, DefaultApprovalTTL) {
		t.Fatal("a one hour old request must not be expired")
	}
	if !Expired(&cr, created.Add(73*time.Hour), DefaultApprovalTTL) {
		t.Fatal("a request without a window expires after 72h")
	}
	end := testNow.Add(30 * time.Minute)
	cr.Spec.Window = &api.ChangeWindow{Start: metav1.NewTime(testNow), End: metav1.NewTime(end)}
	if got := Deadline(&cr, DefaultApprovalTTL); !got.Equal(end) {
		t.Fatalf("approval expires at the window end, got %v", got)
	}
	if Expired(&cr, end.Add(-time.Minute), DefaultApprovalTTL) || !Expired(&cr, end.Add(time.Minute), DefaultApprovalTTL) {
		t.Fatal("expiry must follow the window end")
	}
}

func TestSupersedeWinner(t *testing.T) {
	mk := func(name string, phase api.ChangePhase, age time.Duration) api.ChangeRequest {
		return changeRequest(name, promoteSpec(api.ModelNormal, "v1"), phase, testNow.Add(-age))
	}
	t.Run("newest wins", func(t *testing.T) {
		crs := []api.ChangeRequest{mk("old", api.PhasePending, time.Hour), mk("new", api.PhasePending, time.Minute)}
		if w := SupersedeWinner(crs); w == nil || w.Name != "new" {
			t.Fatalf("expected new, got %v", w)
		}
	})
	t.Run("tie breaks by name", func(t *testing.T) {
		a, b := mk("aaa", api.PhasePending, time.Hour), mk("bbb", api.PhasePending, time.Hour)
		if w := SupersedeWinner([]api.ChangeRequest{a, b}); w == nil || w.Name != "bbb" {
			t.Fatalf("expected bbb, got %v", w)
		}
	})
	t.Run("applying and terminal are excluded", func(t *testing.T) {
		crs := []api.ChangeRequest{mk("applying", api.PhaseApplying, time.Minute), mk("done", api.PhaseApplied, time.Minute), mk("pending", api.PhasePending, time.Hour)}
		if w := SupersedeWinner(crs); w == nil || w.Name != "pending" {
			t.Fatalf("expected pending, got %v", w)
		}
		if w := SupersedeWinner(crs[:2]); w != nil {
			t.Fatalf("no supersedable change, expected nil, got %v", w)
		}
	})
}

func TestBackoff(t *testing.T) {
	if Backoff(0) != 5*time.Second || Backoff(1) != 10*time.Second || Backoff(3) != 40*time.Second {
		t.Fatal("unexpected backoff progression")
	}
	if Backoff(30) != 5*time.Minute {
		t.Fatal("backoff must be capped")
	}
}
