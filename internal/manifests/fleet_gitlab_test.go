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
	for _, want := range []string{"postgres-gitlab-rw.tenant-aep.svc", "rfs-redis-gitlab.tenant-aep.svc", "rediss://", "sslmode: verify-full", "port: 26379"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("missing service contract %s", want)
		}
	}
	for _, extra := range [][]string{{"--set", "gitlab.global.ingress.class="}, {"--set", "postgresMajorVersion=15"}, {"--set", "gitlab.global.psql.sslMode=disable"}} {
		if out, err := exec.Command("helm", append(append([]string{}, args...), extra...)...).CombinedOutput(); err == nil {
			t.Fatalf("unsafe values accepted: %v %s", extra, out)
		}
	}
}
