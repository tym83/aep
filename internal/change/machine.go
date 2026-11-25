package change

import (
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
)

const DefaultApprovalTTL = 72 * time.Hour

// Deadline is when an approval (or a pending request) stops being valid: the
// window end when a window is set, otherwise a fixed TTL after creation.
func Deadline(cr *api.ChangeRequest, ttl time.Duration) time.Time {
	if cr.Spec.Window != nil {
		return cr.Spec.Window.End.Time
	}
	return cr.CreationTimestamp.Add(ttl)
}

func Expired(cr *api.ChangeRequest, now time.Time, ttl time.Duration) bool {
	return now.After(Deadline(cr, ttl))
}

// Supersedable reports whether a phase can still be replaced by a newer
// change for the same target. Once applying, a change finishes; the newer
// change's stale check then sees the modified binding.
func Supersedable(p api.ChangePhase) bool {
	switch p {
	case api.PhasePending, api.PhaseApproved, api.PhaseScheduled:
		return true
	}
	return false
}

// SupersedeWinner picks the surviving change among same-key supersedable
// ones: the newest creation timestamp wins, ties break by name so every
// reconciler instance computes the same winner.
func SupersedeWinner(crs []api.ChangeRequest) *api.ChangeRequest {
	var winner *api.ChangeRequest
	for i := range crs {
		c := &crs[i]
		if !Supersedable(c.Status.Phase) {
			continue
		}
		if winner == nil || c.CreationTimestamp.After(winner.CreationTimestamp.Time) ||
			(c.CreationTimestamp.Equal(&winner.CreationTimestamp) && c.Name > winner.Name) {
			winner = c
		}
	}
	return winner
}

// Backoff grows exponentially between apply attempts and is capped, so a
// failing apply does not hammer the OpenChoreo API.
func Backoff(attempts int32) time.Duration {
	d := 5 * time.Second
	for i := int32(0); i < attempts && d < 5*time.Minute; i++ {
		d *= 2
	}
	return min(d, 5*time.Minute)
}
