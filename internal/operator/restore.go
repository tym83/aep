package operator

import (
	"context"
	"fmt"
	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/kinds"
	"github.com/tym83/aep/internal/policy"
	core "k8s.io/api/core/v1"
	errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"net"
	ctrl "sigs.k8s.io/controller-runtime"
	"strconv"
	"time"
)

const verificationLabel = "aep.aenix.io/restore-verification"

// RestoreVerifier is leader-elected independently of organization readiness.
// Persistent reports are the state machine journal, including across month changes.
type RestoreVerifier struct {
	Reconciler *Reconciler
	Check      func(context.Context, string, int32) error
}

func (v *RestoreVerifier) NeedLeaderElection() bool { return true }
func (v *RestoreVerifier) Start(ctx context.Context) error {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if e := v.Tick(ctx, time.Now().UTC()); e != nil {
			ctrl.LoggerFrom(ctx).Error(e, "restore verification")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
func (v *RestoreVerifier) Tick(ctx context.Context, now time.Time) error {
	r := v.Reconciler
	reports, e := r.Management.Kube.CoreV1().ConfigMaps(r.Options.Namespace).List(ctx, metav1.ListOptions{LabelSelector: verificationLabel + "=true"})
	if e != nil {
		return e
	}
	var first error
	for i := range reports.Items {
		if e = v.advance(ctx, &reports.Items[i], now); e != nil && first == nil {
			first = e
		}
	}
	var orgs api.OrganizationList
	if e = r.List(ctx, &orgs); e != nil {
		return e
	}
	for _, org := range orgs.Items {
		if !org.DeletionTimestamp.IsZero() {
			continue
		}
		tier, ok := org.Spec.Tiers["prod"]
		if !ok || tier.Services.BackupPolicy == "" {
			continue
		}
		ns := TenantNamespace(org.Name, "prod")
		for kind, m := range kinds.Table {
			apps, e := r.Management.list(ctx, appsAPI, m.Resource, ns)
			if e != nil {
				return e
			}
			for _, app := range apps {
				if app.GetLabels()["aep.aenix.io/managed-by"] != "bridge" {
					continue
				}
				name := policy.Name(string(org.UID), string(app.GetUID()), now.Format("2006-01"), "restore")
				report := &core.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.Options.Namespace, Labels: map[string]string{verificationLabel: "true", OwnerLabel: string(org.UID)}}, Data: map[string]string{"organization": org.Name, "namespace": ns, "source": app.GetName(), "sourceUID": string(app.GetUID()), "kind": kind, "temporary": name, "month": now.Format("2006-01"), "phase": "Pending", "started": now.Format(time.RFC3339)}}
				_, e = r.Management.Kube.CoreV1().ConfigMaps(r.Options.Namespace).Create(ctx, report, metav1.CreateOptions{})
				if e != nil && !errors.IsAlreadyExists(e) {
					return e
				}
			}
		}
	}
	return first
}
func (v *RestoreVerifier) save(ctx context.Context, report *core.ConfigMap) error {
	_, e := v.Reconciler.Management.Kube.CoreV1().ConfigMaps(report.Namespace).Update(ctx, report, metav1.UpdateOptions{})
	return e
}
func (v *RestoreVerifier) advance(ctx context.Context, report *core.ConfigMap, now time.Time) error {
	d := report.Data
	if d["phase"] == "Complete" {
		return nil
	}
	r := v.Reconciler
	m, ok := kinds.Table[d["kind"]]
	if !ok {
		return fmt.Errorf("unknown verification kind")
	}
	ns, name := d["namespace"], d["temporary"]
	if ns != TenantNamespace(d["organization"], "prod") || name != report.Name || name == d["source"] {
		return fmt.Errorf("unsafe verification target")
	}
	apps := r.Management.Dynamic.Resource(m.GVR()).Namespace(ns)
	jobs := r.Management.resource(backupsAPI, "restorejobs", ns)
	target, e := apps.Get(ctx, name, metav1.GetOptions{})
	if e != nil && !errors.IsNotFound(e) {
		return e
	}
	if e == nil && (target.GetLabels()[verificationLabel] != string(report.UID) || target.GetUID() == types.UID(d["sourceUID"])) {
		return fmt.Errorf("refusing unowned verification target")
	}
	job, je := jobs.Get(ctx, name, metav1.GetOptions{})
	if je != nil && !errors.IsNotFound(je) {
		return je
	}
	if je == nil && job.GetLabels()[verificationLabel] != string(report.UID) {
		return fmt.Errorf("refusing unowned RestoreJob")
	}
	if d["phase"] == "Cleanup" {
		// Stop the restore before deleting its destination. Keep the report indefinitely.
		if je == nil {
			uid := job.GetUID()
			foreground := metav1.DeletePropagationForeground
			return jobs.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}, PropagationPolicy: &foreground})
		}
		if e == nil {
			uid := target.GetUID()
			return apps.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
		}
		d["phase"] = "Complete"
		d["completed"] = now.Format(time.RFC3339)
		return v.save(ctx, report)
	}
	started, err := time.Parse(time.RFC3339, d["started"])
	if err != nil {
		return err
	}
	if now.Sub(started) > 6*time.Hour {
		d["result"] = "Failed"
		d["message"] = "Restore readiness or connection check exceeded six hours"
		d["phase"] = "Cleanup"
		return v.save(ctx, report)
	}
	if d["backup"] == "" {
		backups, e := r.Management.list(ctx, backupsAPI, "backups", ns)
		if e != nil {
			return e
		}
		var newest *unstructured.Unstructured
		var taken time.Time
		for i := range backups {
			b := &backups[i]
			bk, _, _ := unstructured.NestedString(b.Object, "spec", "applicationRef", "kind")
			bn, _, _ := unstructured.NestedString(b.Object, "spec", "applicationRef", "name")
			phase, _, _ := unstructured.NestedString(b.Object, "status", "phase")
			ts, _, _ := unstructured.NestedString(b.Object, "spec", "takenAt")
			t, err := time.Parse(time.RFC3339, ts)
			if bk == d["kind"] && bn == d["source"] && phase == "Ready" && err == nil && (newest == nil || t.After(taken)) {
				newest = b
				taken = t
			}
		}
		if newest == nil {
			return nil
		}
		d["backup"] = newest.GetName()
		return v.save(ctx, report)
	}
	if errors.IsNotFound(e) {
		source, e := apps.Get(ctx, d["source"], metav1.GetOptions{})
		if e != nil {
			return e
		}
		if string(source.GetUID()) != d["sourceUID"] {
			return fmt.Errorf("source was replaced during verification")
		}
		sourceSpec, _, _ := unstructured.NestedMap(source.Object, "spec")
		spec := M{}
		// Do not copy exposure, backup schedules or integration settings into a drill.
		for _, key := range []string{"version", "flavor", "resourcesPreset", "resources", "size", "replicas", "users", "databases", "tls"} {
			if value, ok := sourceSpec[key]; ok {
				spec[key] = value
			}
		}
		target = obj(appsAPI, d["kind"], ns, name, spec)
		target.SetLabels(map[string]string{verificationLabel: string(report.UID), "aep.aenix.io/organization": d["organization"], "aep.aenix.io/tier": "prod"})
		_, e = apps.Create(ctx, target, metav1.CreateOptions{})
		return e
	}
	if errors.IsNotFound(je) {
		if !ready(target) {
			return nil
		}
		job = obj(backupsAPI, "RestoreJob", ns, name, M{"backupRef": M{"name": d["backup"]}, "targetApplicationRef": M{"apiGroup": "apps.cozystack.io", "kind": d["kind"], "name": name}})
		job.SetLabels(map[string]string{verificationLabel: string(report.UID)})
		_, e = jobs.Create(ctx, job, metav1.CreateOptions{})
		return e
	}
	phase, _, _ := unstructured.NestedString(job.Object, "status", "phase")
	if phase == "Failed" {
		d["result"] = "Failed"
		d["message"] = "RestoreJob failed"
		d["phase"] = "Cleanup"
		return v.save(ctx, report)
	}
	if phase != "Succeeded" || !ready(target) {
		return nil
	}
	svc, e := r.Management.Kube.CoreV1().Services(ns).Get(ctx, m.Service(name), metav1.GetOptions{})
	if e != nil {
		return e
	}
	host := svc.Spec.ClusterIP
	if net.ParseIP(host) == nil {
		return fmt.Errorf("temporary service has no ClusterIP")
	}
	check := v.Check
	if check == nil {
		check = func(ctx context.Context, host string, port int32) error {
			dialer := net.Dialer{Timeout: 5 * time.Second}
			conn, e := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
			if e == nil {
				conn.Close()
			}
			return e
		}
	}
	if e = check(ctx, host, m.Port); e != nil {
		return nil
	}
	d["result"] = "Passed"
	d["message"] = "Restore succeeded, application Ready, TCP connection accepted"
	d["phase"] = "Cleanup"
	return v.save(ctx, report)
}
