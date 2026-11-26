package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/tym83/aep/internal/policy"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
)

func resource(group, version, name string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: group, Version: version, Resource: name}
}

var hr = resource("helm.toolkit.fluxcd.io", "v2", "helmreleases")
var events = resource("", "v1", "events")
var quotas = resource("", "v1", "resourcequotas")
var appResources = map[string]string{"Postgres": "postgreses", "Redis": "redises", "Kubernetes": "kuberneteses", "Kafka": "kafkas", "RabbitMQ": "rabbitmqs", "MySQL": "mysqls", "ClickHouse": "clickhouses"}

func namespace(org, tier string) string { return "tenant-" + org + "-" + tier }
func (s *Server) list(ctx context.Context, e *Evidence, g schema.GroupVersionResource, ns string) []unstructured.Unstructured {
	// Bound backend work as well as output. A partial list is always explicit.
	list, err := s.Client.Resource(g).Namespace(ns).List(ctx, metav1.ListOptions{Limit: 1000})
	if err != nil {
		e.Unavailable = append(e.Unavailable, ns+"/"+g.Resource)
		return nil
	}
	if list.GetContinue() != "" || len(list.Items) > 1000 {
		e.Truncated = true
	}
	if len(list.Items) > 1000 {
		return list.Items[:1000]
	}
	return list.Items
}
func value(o map[string]any, p ...string) any {
	v, _, _ := unstructured.NestedFieldCopy(o, p...)
	return v
}
func project(o unstructured.Unstructured) map[string]any {
	r := map[string]any{"name": o.GetName(), "namespace": o.GetNamespace(), "kind": o.GetKind(), "generation": o.GetGeneration(), "createdAt": o.GetCreationTimestamp().Format(time.RFC3339)}
	// Only evidence fields: arbitrary specs, connection strings, addresses,
	// pod templates and referenced secret contents never leave this boundary.
	status := map[string]any{}
	for _, k := range []string{"conditions", "phase", "currentPrimary", "targetPrimary", "instances", "readyInstances", "used", "hard", "observedGeneration", "lastAttemptedRevision", "lastAppliedRevision", "lastAttemptedConfigDigest", "history", "completionTimestamp", "startTimestamp"} {
		if v := value(o.Object, "status", k); v != nil {
			status[k] = v
		}
	}
	if o.GetKind() == "TenantControlPlane" {
		deployment := map[string]any{}
		for _, k := range []string{"replicas", "readyReplicas", "availableReplicas", "unavailableReplicas", "updatedReplicas", "lastUpdate", "observedGeneration"} {
			deployment[k] = value(o.Object, "status", "kubernetesResources", "deployment", k)
		}
		conditions, _ := value(o.Object, "status", "kubernetesResources", "deployment", "conditions").([]any)
		deployment["conditions"] = selectFields(conditions, []string{"type", "status", "reason", "message", "lastTransitionTime", "lastUpdateTime"})
		status["kubernetesResources"] = map[string]any{"version": value(o.Object, "status", "kubernetesResources", "version"), "deployment": deployment}
	}
	if history, ok := status["history"].([]any); ok {
		status["history"] = selectFields(history, []string{"version", "chartName", "chartVersion", "configDigest", "digest", "firstDeployed", "lastDeployed", "status"})
	}
	if conditions, ok := status["conditions"].([]any); ok {
		status["conditions"] = selectFields(conditions, []string{"type", "status", "reason", "message", "lastTransitionTime", "observedGeneration"})
	}
	if o.GetKind() == "Backup" {
		r["takenAt"] = value(o.Object, "spec", "takenAt")
	}
	r["status"] = status
	a := map[string]any{}
	for _, k := range []string{"claim-ref", "claim-uid", "cluster-id", "applied-spec-hash", "claim-spec-hash", "immutable-spec-hash", "backup-policy", "paused", "orphaned", "resource", "environment", "project", "eval-simulated-failure"} {
		if v := o.GetAnnotations()["aep.aenix.io/"+k]; v != "" {
			a[k] = v
		}
	}
	r["bridge"] = a
	return r
}
func (s *Server) query(ctx context.Context, name string, a Args) (Evidence, error) {
	e := Evidence{Rows: []map[string]any{}}
	sc, ok := ctx.Value(contextKey{}).(scope)
	if !ok || sc.org != a.Org {
		return e, fmt.Errorf("organization mismatch")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	tierOK := func(t string) bool {
		for _, v := range sc.tiers {
			if v == t && len(validation.IsDNS1123Label(t)) == 0 {
				return true
			}
		}
		return false
	}
	switch name {
	case "infra_changes", "quota_and_denials":
		until := time.Now().UTC()
		since := until.Add(-24 * time.Hour)
		var err error
		if a.Since != "" {
			since, err = time.Parse(time.RFC3339, a.Since)
			if err != nil {
				return e, fmt.Errorf("invalid since")
			}
		}
		if a.Until != "" {
			until, err = time.Parse(time.RFC3339, a.Until)
			if err != nil {
				return e, fmt.Errorf("invalid until")
			}
		}
		if a.Since == "" {
			since = until.Add(-24 * time.Hour)
		}
		if !since.Before(until) || until.Sub(since) > 7*24*time.Hour {
			return e, fmt.Errorf("time window must be positive and at most seven days")
		}
		for _, tier := range sc.tiers {
			if !tierOK(tier) {
				continue
			}
			ns := namespace(sc.org, tier)
			if name == "quota_and_denials" {
				for _, o := range s.list(ctx, &e, quotas, ns) {
					e.Rows = append(e.Rows, project(o))
				}
			}
			for _, o := range s.list(ctx, &e, events, ns) {
				reason, _ := o.Object["reason"].(string)
				message, _ := o.Object["message"].(string)
				if name == "quota_and_denials" && ((reason != "FailedCreate" && reason != "Forbidden") || !strings.Contains(strings.ToLower(message), "quota")) {
					continue
				}
				ts := eventTime(o)
				if ts.Before(since) || ts.After(until) {
					continue
				}
				ref, _ := o.Object["involvedObject"].(map[string]any)
				if ref["kind"] == "Secret" || ref["kind"] == "TenantSecret" {
					continue
				}
				e.Rows = append(e.Rows, map[string]any{"kind": "Event", "namespace": ns, "name": o.GetName(), "time": ts.Format(time.RFC3339Nano), "reason": reason, "message": message, "object": ref, "count": o.Object["count"]})
			}
			if name == "infra_changes" {
				for _, kind := range []string{"Postgres", "Redis", "Kubernetes"} {
					for _, app := range s.list(ctx, &e, resource("apps.cozystack.io", "v1alpha1", appResources[kind]), ns) {
						// Managed fields date the last observed update; this is a snapshot,
						// not an invented historical spec diff.
						changed := app.GetCreationTimestamp().Time
						for _, field := range app.GetManagedFields() {
							if field.Time != nil && field.Time.Time.After(changed) {
								changed = field.Time.Time
							}
						}
						if changed.Before(since) || changed.After(until) {
							continue
						}
						row := project(app)
						row["kind"] = "AppChangeObservation"
						row["applicationKind"] = kind
						row["time"] = changed.Format(time.RFC3339Nano)
						spec, _, _ := nestedMap(app.Object, "spec")
						row["specDigest"] = policy.Hash(spec)
						row["historyAvailable"] = false
						e.Rows = append(e.Rows, row)
					}
				}
				for _, o := range s.list(ctx, &e, hr, ns) {
					history, _ := value(o.Object, "status", "history").([]any)
					for _, h := range history {
						m, ok := h.(map[string]any)
						if !ok {
							continue
						}
						ts, _ := m["lastDeployed"].(string)
						t, err := time.Parse(time.RFC3339, ts)
						if err != nil || t.Before(since) || t.After(until) {
							continue
						}
						e.Rows = append(e.Rows, map[string]any{"kind": "ReleaseRevision", "namespace": ns, "name": o.GetName(), "time": ts, "revision": m["version"], "chartVersion": m["chartVersion"], "configDigest": m["configDigest"], "digest": m["digest"], "status": m["status"]})
					}
				}
			}
		}
		sort.SliceStable(e.Rows, func(i, j int) bool {
			a, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(e.Rows[i]["time"]))
			b, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(e.Rows[j]["time"]))
			return a.After(b)
		})
	case "managed_service_state":
		parts := strings.Split(a.Service, "/")
		if len(parts) != 3 || !tierOK(parts[0]) || appResources[parts[1]] == "" || len(validation.IsDNS1123Subdomain(parts[2])) != 0 {
			return e, fmt.Errorf("service must be tier/kind/name within the organization")
		}
		ns := namespace(sc.org, parts[0])
		service := parts[2]
		release := strings.ToLower(parts[1]) + "-" + service
		app, err := s.Client.Resource(resource("apps.cozystack.io", "v1alpha1", appResources[parts[1]])).Namespace(ns).Get(ctx, service, metav1.GetOptions{})
		if err != nil {
			return e, fmt.Errorf("service unavailable")
		}
		e.Rows = append(e.Rows, project(*app))
		releaseObj, err := s.Client.Resource(hr).Namespace(ns).Get(ctx, release, metav1.GetOptions{})
		if err == nil {
			e.Rows = append(e.Rows, project(*releaseObj))
		} else {
			e.Unavailable = append(e.Unavailable, "HelmRelease")
		}
		if parts[1] == "Postgres" {
			cluster, err := s.Client.Resource(resource("postgresql.cnpg.io", "v1", "clusters")).Namespace(ns).Get(ctx, release, metav1.GetOptions{})
			if err == nil {
				e.Rows = append(e.Rows, project(*cluster))
			} else {
				e.Unavailable = append(e.Unavailable, "CNPG Cluster")
			}
			if s.config.MetricsURL != "" {
				s.metrics(ctx, &e, sc.org, ns, release)
			} else {
				e.Unavailable = append(e.Unavailable, "PVC usage: metrics URL not configured")
			}
		}
		backups := s.list(ctx, &e, resource("backups.cozystack.io", "v1alpha1", "backups"), ns)
		sort.Slice(backups, func(i, j int) bool {
			return backupTime(backups[j]).Before(backupTime(backups[i]))
		})
		foundBackup := false
		for _, b := range backups {
			if backupMatches(b, parts[1], service) {
				foundBackup = true
				e.Rows = append(e.Rows, project(b))
				break
			}
		}
		if !foundBackup {
			e.Unavailable = append(e.Unavailable, "No matching Backup observed")
		}
	case "cluster_health":
		if !tierOK(a.Tier) {
			return e, fmt.Errorf("unknown tier")
		}
		ns := namespace(sc.org, a.Tier)
		for _, g := range []schema.GroupVersionResource{resource("kamaji.clastix.io", "v1alpha1", "tenantcontrolplanes"), resource("apps.cozystack.io", "v1alpha1", "kubernetesnodeses"), resource("kubevirt.io", "v1", "virtualmachineinstances"), quotas} {
			for _, o := range s.list(ctx, &e, g, ns) {
				e.Rows = append(e.Rows, project(o))
			}
		}
		// Guest Node objects live behind the tenant API, not the management API.
		// Never substitute management Nodes (which disclose physical hosts).
		if s.config.MetricsURL != "" {
			s.nodeMetrics(ctx, &e, sc.org, ns)
		} else {
			e.Unavailable = append(e.Unavailable, "Guest node pressure: requires tenant metrics")
		}
	default:
		return e, fmt.Errorf("unknown tool")
	}
	return s.bounded(e), nil
}
func eventTime(o unstructured.Unstructured) time.Time {
	for _, p := range [][]string{{"series", "lastObservedTime"}, {"lastTimestamp"}, {"eventTime"}, {"metadata", "creationTimestamp"}} {
		s, _ := value(o.Object, p...).(string)
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
func backupMatches(b unstructured.Unstructured, kind, name string) bool {
	return value(b.Object, "spec", "applicationRef", "name") == name && value(b.Object, "spec", "applicationRef", "kind") == kind && value(b.Object, "spec", "applicationRef", "apiGroup") == "apps.cozystack.io"
}
func (s *Server) metrics(ctx context.Context, e *Evidence, org, ns, release string) {
	// A fixed query, bounded by tenant namespace and exact CNPG PVC names found
	// through labels; user input can never supply PromQL or an endpoint.
	for _, pvc := range s.list(ctx, e, resource("", "v1", "persistentvolumeclaims"), ns) {
		if pvc.GetLabels()["cnpg.io/cluster"] != release {
			continue
		}
		q := fmt.Sprintf(`kubelet_volume_stats_used_bytes{namespace=%q,persistentvolumeclaim=%q} / kubelet_volume_stats_capacity_bytes{namespace=%q,persistentvolumeclaim=%q}`, ns, pvc.GetName(), ns, pvc.GetName())
		s.metricQuery(ctx, e, org, ns, q, "PVCUsage")
	}
}
func (s *Server) nodeMetrics(ctx context.Context, e *Evidence, org, ns string) {
	s.metricQuery(ctx, e, org, ns, fmt.Sprintf(`kube_node_status_condition{tenant_namespace=%q,condition=~"MemoryPressure|DiskPressure|PIDPressure",status="true"}`, ns), "GuestNodePressure")
}
func (s *Server) metricQuery(ctx context.Context, e *Evidence, org, ns, q, kind string) {
	endpoint := strings.ReplaceAll(strings.ReplaceAll(s.config.MetricsURL, "{org}", org), "{namespace}", ns)
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		e.Unavailable = append(e.Unavailable, "invalid metrics endpoint")
		return
	}
	args := u.Query()
	args.Set("query", q)
	u.RawQuery = args.Encode()
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return
	}
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		e.Unavailable = append(e.Unavailable, kind)
		return
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if err != nil || len(b) > 65536 || resp.StatusCode != 200 {
		e.Unavailable = append(e.Unavailable, kind)
		return
	}
	var data struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  []any             `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal(b, &data) != nil || data.Status != "success" {
		e.Unavailable = append(e.Unavailable, kind)
		return
	}
	samples := 0
	for _, r := range data.Data.Result {
		// Defense in depth against a misconfigured shared metrics endpoint.
		if kind == "PVCUsage" && r.Metric["namespace"] != ns {
			continue
		}
		if kind == "GuestNodePressure" && r.Metric["tenant_namespace"] != ns {
			continue
		}
		samples++
		e.Rows = append(e.Rows, map[string]any{"kind": kind, "namespace": ns, "pvc": r.Metric["persistentvolumeclaim"], "node": r.Metric["node"], "condition": r.Metric["condition"], "sample": r.Value})
	}
	if samples == 0 {
		e.Unavailable = append(e.Unavailable, kind+": no scoped samples")
	}
}

func backupTime(o unstructured.Unstructured) time.Time {
	if s, ok := value(o.Object, "spec", "takenAt").(string); ok {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t
		}
	}
	return o.GetCreationTimestamp().Time
}
func selectFields(items []any, keys []string) []any {
	out := []any{}
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		row := map[string]any{}
		for _, k := range keys {
			if v, ok := m[k]; ok {
				row[k] = v
			}
		}
		out = append(out, row)
	}
	return out
}
