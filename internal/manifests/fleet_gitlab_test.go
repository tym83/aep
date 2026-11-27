package manifests

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestGitLabRestrictedProfile(t *testing.T) {
	if _, e := os.Stat("../../charts/aep-gitlab/charts/gitlab-10.6.2.tgz"); e != nil {
		t.Skip("run helm dependency build charts/aep-gitlab")
	}
	b, e := exec.Command("helm", "template", "gitlab", "../../charts/aep-gitlab", "--namespace", "tenant-aep", "--set", "gitlab.global.ingress.class=nginx").CombinedOutput()
	if e != nil {
		t.Fatalf("%v %s", e, b)
	}
	count := 0
	for _, d := range decode(t, b) {
		spec, _ := d["spec"].(map[string]any)
		switch d["kind"] {
		case "Deployment", "StatefulSet", "DaemonSet", "Job":
			template, _ := spec["template"].(map[string]any)
			spec, _ = template["spec"].(map[string]any)
		case "CronJob":
			j, _ := spec["jobTemplate"].(map[string]any)
			js, _ := j["spec"].(map[string]any)
			template, _ := js["template"].(map[string]any)
			spec, _ = template["spec"].(map[string]any)
		case "Pod":
		default:
			continue
		}
		count++
		podSC, _ := spec["securityContext"].(map[string]any)
		seccomp, _ := podSC["seccompProfile"].(map[string]any)
		for _, key := range []string{"containers", "initContainers"} {
			items, _ := spec[key].([]any)
			for _, v := range items {
				c := v.(map[string]any)
				sc, _ := c["securityContext"].(map[string]any)
				if sc["runAsNonRoot"] != true && podSC["runAsNonRoot"] != true {
					t.Fatalf("%s %s lacks nonroot", d["kind"], c["name"])
				}
				if sc["allowPrivilegeEscalation"] != false {
					t.Fatalf("%s permits escalation", c["name"])
				}
				caps, _ := sc["capabilities"].(map[string]any)
				drop, _ := caps["drop"].([]any)
				all := false
				for _, x := range drop {
					all = all || x == "ALL"
				}
				if !all {
					t.Fatalf("%s capabilities", c["name"])
				}
				ownSeccomp, _ := sc["seccompProfile"].(map[string]any)
				if seccomp["type"] != "RuntimeDefault" && ownSeccomp["type"] != "RuntimeDefault" {
					t.Fatalf("%s missing seccomp", c["name"])
				}
				image, _ := c["image"].(string)
				if !strings.Contains(image, ":") || strings.HasSuffix(image, ":latest") {
					t.Fatalf("unpinned %s", image)
				}
			}
		}
	}
	if count == 0 {
		t.Fatal("no pods checked")
	}
}

func TestGitLabExternalServiceContract(t *testing.T) {
	args := []string{"template", "gitlab", "../../charts/aep-gitlab", "--set", "gitlab.global.ingress.class=nginx"}
	b, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, b)
	}
	for _, want := range []string{"postgres-gitlab-rw.tenant-aep.svc", "rediss://rfrm-redis-gitlab.tenant-aep.svc", "sslmode: verify-full", "gitlab-s3-lfs"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("missing service contract %s", want)
		}
	}
	// Sentinel returns pod IPs that the Cozystack Redis certificate does not cover.
	if strings.Contains(string(b), "26379") {
		t.Fatal("Redis Sentinel endpoint rendered")
	}
	for _, extra := range [][]string{{"--set", "gitlab.global.ingress.class="}, {"--set", "postgresMajorVersion=15"}, {"--set", "gitlab.global.psql.sslMode=disable"}} {
		if out, err := exec.Command("helm", append(append([]string{}, args...), extra...)...).CombinedOutput(); err == nil {
			t.Fatalf("unsafe values accepted: %v %s", extra, out)
		}
	}
}

func TestOperatorCanFinalizeFleetResources(t *testing.T) {
	b, err := exec.Command("helm", "template", "operator", "../../charts/aep-operator", "--set", "aepcpKubeconfigSecret=cp/access").CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, b)
	}
	needed := map[string]bool{"work.open-cluster-management.io/manifestworks": false, "multicluster.x-k8s.io/clusterprofiles": false, "aep.aenix.io/externalclusters": false, "rbac.authorization.k8s.io/clusterroles": false, "rbac.authorization.k8s.io/clusterrolebindings": false}
	for _, m := range decode(t, b) {
		if m["kind"] != "ClusterRole" {
			continue
		}
		rules, _ := m["rules"].([]any)
		for _, raw := range rules {
			r := raw.(map[string]any)
			verbs, _ := r["verbs"].([]any)
			hasDelete := false
			for _, v := range verbs {
				hasDelete = hasDelete || v == "delete"
			}
			if !hasDelete {
				continue
			}
			for _, g := range r["apiGroups"].([]any) {
				for _, resource := range r["resources"].([]any) {
					key := g.(string) + "/" + resource.(string)
					if _, ok := needed[key]; ok {
						needed[key] = true
					}
				}
			}
		}
	}
	for key, allowed := range needed {
		if !allowed {
			t.Fatal("finalizer cannot delete", key)
		}
	}
}

// Only the umbrella Jobs that write Secrets may reach the Kubernetes API in a
// Cozystack tenant; long-running GitLab pods must not carry the access label.
func TestGitLabAPIAccessLabelIsScoped(t *testing.T) {
	if _, e := os.Stat("../../charts/aep-gitlab/charts/gitlab-10.6.2.tgz"); e != nil {
		t.Skip("run helm dependency build charts/aep-gitlab")
	}
	b, e := exec.Command("helm", "template", "gitlab", "../../charts/aep-gitlab", "--namespace", "tenant-aep", "--set", "gitlab.global.ingress.class=nginx").CombinedOutput()
	if e != nil {
		t.Fatalf("%v %s", e, b)
	}
	labelled := map[string]bool{}
	for _, d := range decode(t, b) {
		spec, _ := d["spec"].(map[string]any)
		template, _ := spec["template"].(map[string]any)
		meta, _ := template["metadata"].(map[string]any)
		labels, _ := meta["labels"].(map[string]any)
		if labels["policy.cozystack.io/allow-to-apiserver"] == "true" {
			m, _ := d["metadata"].(map[string]any)
			labelled[d["kind"].(string)+"/"+m["name"].(string)] = true
		}
	}
	shared := false
	for name := range labelled {
		if !strings.HasPrefix(name, "Job/") {
			t.Errorf("%s may reach the Kubernetes API", name)
		}
		shared = shared || strings.Contains(name, "shared-secrets")
	}
	if !shared {
		t.Fatal("shared-secrets Job cannot reach the Kubernetes API in a Cozystack tenant")
	}
}

// Cozystack tenant LimitRanges give unset containers default limits below
// GitLab's requests, which makes the Pods invalid; the AEP baseline also
// requires limits. Every main container must carry explicit CPU and memory limits.
func TestGitLabContainersHaveLimits(t *testing.T) {
	if _, e := os.Stat("../../charts/aep-gitlab/charts/gitlab-10.6.2.tgz"); e != nil {
		t.Skip("run helm dependency build charts/aep-gitlab")
	}
	b, e := exec.Command("helm", "template", "gitlab", "../../charts/aep-gitlab", "--namespace", "tenant-aep", "--set", "gitlab.global.ingress.class=nginx").CombinedOutput()
	if e != nil {
		t.Fatalf("%v %s", e, b)
	}
	for _, d := range decode(t, b) {
		spec, _ := d["spec"].(map[string]any)
		switch d["kind"] {
		case "Deployment", "StatefulSet", "Job":
			template, _ := spec["template"].(map[string]any)
			spec, _ = template["spec"].(map[string]any)
		case "CronJob":
			j, _ := spec["jobTemplate"].(map[string]any)
			js, _ := j["spec"].(map[string]any)
			template, _ := js["template"].(map[string]any)
			spec, _ = template["spec"].(map[string]any)
		default:
			continue
		}
		m, _ := d["metadata"].(map[string]any)
		containers, _ := spec["containers"].([]any)
		for _, v := range containers {
			c := v.(map[string]any)
			r, _ := c["resources"].(map[string]any)
			l, _ := r["limits"].(map[string]any)
			if l["cpu"] == nil || l["memory"] == nil {
				t.Errorf("%s/%v container %v has no CPU and memory limits", d["kind"], m["name"], c["name"])
			}
		}
	}
}
