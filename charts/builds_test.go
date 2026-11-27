package charts

import (
	"strings"
	"testing"
)

var buildControl = []string{"organizations[0].namespace=acme", "organizations[0].workflowPlane=nonprod", "registry.host=harbor.example", "openchoreo.apiUrl=https://api.example", "openchoreo.tokenUrl=https://id.example/token"}

func TestBuildsControlPlane(t *testing.T) {
	out, err := render(t, "aep-builds", buildControl...)
	if err != nil {
		t.Fatal(out, err)
	}
	docs := mcpDocuments(t, out)
	if len(docs) != 1 || docs[0]["kind"] != "Workflow" {
		t.Fatalf("want one namespaced Workflow, got %v", docs)
	}
	meta := docs[0]["metadata"].(map[string]any)
	spec := docs[0]["spec"].(map[string]any)
	if meta["namespace"] != "acme" || meta["labels"].(map[string]any)["openchoreo.dev/workflow-type"] != "component" {
		t.Fatal(meta)
	}
	// The organization's own plane, never a shared ClusterWorkflowPlane.
	if ref := spec["workflowPlaneRef"].(map[string]any); ref["kind"] != "WorkflowPlane" || ref["name"] != "nonprod" {
		t.Fatal(ref)
	}
	for _, want := range []string{"harbor.example/${metadata.namespaceName}/", `"aep-acme-publisher"`, "templateRef: {name: aep-kaniko-build"} {
		if !strings.Contains(out, want) && !strings.Contains(out, strings.Trim(want, `"`)) {
			t.Fatalf("missing %s", want)
		}
	}
	for _, missing := range [][]string{{"registry.host=", "registry.host is required"}, {"openchoreo.apiUrl=", "openchoreo.apiUrl is required"}} {
		values := append(append([]string{}, buildControl...), missing[0])
		if out, err := render(t, "aep-builds", values...); err == nil || !strings.Contains(out, missing[1]) {
			t.Fatalf("%s accepted: %s", missing[0], out)
		}
	}
	if out, err := render(t, "aep-builds"); err != nil || len(mcpDocuments(t, out)) != 0 {
		t.Fatal("no organizations must render nothing", out, err)
	}
}

func TestBuildsUnprivileged(t *testing.T) {
	out, err := render(t, "aep-builds", "plane=workflow")
	if err != nil {
		t.Fatal(out, err)
	}
	docs := mcpDocuments(t, out)
	if len(docs) != 3 {
		t.Fatalf("got %d templates", len(docs))
	}
	for _, d := range docs {
		if d["kind"] != "ClusterWorkflowTemplate" {
			t.Fatal(d["kind"])
		}
		for _, tpl := range d["spec"].(map[string]any)["templates"].([]any) {
			tpl := tpl.(map[string]any)
			containers := []any{tpl["container"]}
			if init, ok := tpl["initContainers"].([]any); ok {
				containers = append(containers, init...)
			}
			for _, c := range containers {
				c := c.(map[string]any)
				sc, _ := c["securityContext"].(map[string]any)
				if sc == nil || sc["privileged"] == true || sc["allowPrivilegeEscalation"] != false {
					t.Fatalf("%s: container %v must disable privilege escalation", tpl["name"], c["image"])
				}
				limits, _ := c["resources"].(map[string]any)["limits"].(map[string]any)
				if limits["cpu"] == nil || limits["memory"] == nil {
					t.Fatalf("%s: container %v needs limits", tpl["name"], c["image"])
				}
				if !strings.Contains(c["image"].(string), "@sha256:") {
					t.Fatalf("mutable image %v", c["image"])
				}
			}
		}
	}
}

func TestNamespacedComponentTypesForBuilds(t *testing.T) {
	out, err := render(t, "aep-component-types", "buildOrganizations={acme}")
	if err != nil {
		t.Fatal(out, err)
	}
	namespaced := 0
	for _, d := range mcpDocuments(t, out) {
		meta := d["metadata"].(map[string]any)
		var workflows []string
		for _, ref := range d["spec"].(map[string]any)["allowedWorkflows"].([]any) {
			ref := ref.(map[string]any)
			workflows = append(workflows, ref["kind"].(string)+"/"+ref["name"].(string))
		}
		joined := strings.Join(workflows, ",")
		switch d["kind"] {
		case "ComponentType":
			namespaced++
			if meta["namespace"] != "acme" || !strings.Contains(joined, "Workflow/aep-kaniko-builder") {
				t.Fatal(meta, joined)
			}
		case "ClusterComponentType":
			// A cluster type may not reference a namespaced Workflow.
			if strings.Contains(joined, ",Workflow/") || strings.HasPrefix(joined, "Workflow/") {
				t.Fatal(joined)
			}
		}
	}
	if namespaced != 4 {
		t.Fatalf("got %d namespaced types", namespaced)
	}
}
