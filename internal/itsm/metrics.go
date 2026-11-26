package itsm

import (
	"encoding/json"
	"fmt"
	corev1 "k8s.io/api/core/v1"
	"net/http"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Metrics expose durable terminal failures without exposing event contents.
func MetricsHandler(engines ...*Engine) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counts := map[string]int{"pending": 0, "completed": 0, "failed": 0, "corrupt": 0}
		for _, e := range engines {
			list := &corev1.ConfigMapList{}
			if err := e.Store.Client.List(r.Context(), list, client.InNamespace(e.Store.Namespace), client.MatchingLabels{"aep.aenix.io/itsm-state": "true", "aep.aenix.io/itsm-scope": Digest([]byte(e.Store.Scope))[:32]}); err != nil {
				http.Error(w, "state unavailable", 503)
				return
			}
			for _, cm := range list.Items {
				var record Record
				if json.Unmarshal([]byte(cm.Data["state"]), &record) != nil || record.Event.Validate() != nil {
					counts["corrupt"]++
					continue
				}
				status := "pending"
				if record.Done {
					status = "completed"
				}
				if record.Failure != 0 {
					status = "failed"
				}
				counts[status]++
			}
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		for _, status := range []string{"pending", "completed", "failed", "corrupt"} {
			fmt.Fprintf(w, "aep_itsm_records{status=%q} %d\n", status, counts[status])
		}
	})
}
