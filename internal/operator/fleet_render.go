package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/tym83/aep/internal/registrymirror"
	buildversion "github.com/tym83/aep/internal/version"
	inventory "github.com/tym83/aep/release"
	"io"
	"k8s.io/apimachinery/pkg/util/yaml"
	"os/exec"
	"path/filepath"
	"strings"
)

// FleetRenderer renders locally staged, version-pinned charts. No credentials
// for a managed cluster are used by Helm or retained on management.
type FleetRenderer interface {
	Render(context.Context, string, string, string, M) ([]any, error)
}
type HelmFleetRenderer struct{ Directory, Mirror string }

func (h HelmFleetRenderer) Render(ctx context.Context, name, version, namespace string, values M) ([]any, error) {
	if values == nil {
		values = M{}
	}
	values = MirrorValues(name, h.Mirror, values)
	if strings.HasPrefix(name, "aep-") {
		setMap(values, []string{"global", "aepVersion"}, buildversion.Version)
	}
	setMap(values, []string{"global", "imageRegistry"}, h.Mirror)
	if name == "openchoreo-data-plane" && h.Mirror != "" {
		setMap(values, []string{"global", "imageRegistry"}, h.Mirror+"/cr.openchoreo.dev")
	}
	input, e := json.Marshal(values)
	if e != nil {
		return nil, e
	}
	cmd := exec.CommandContext(ctx, "helm", "template", name, filepath.Join(h.Directory, name+"-"+version+".tgz"), "--namespace", namespace, "--include-crds", "--kube-version", "1.35.0", "--values", "-")
	cmd.Stdin = bytes.NewReader(input)
	b, e := cmd.Output()
	if e != nil {
		return nil, fmt.Errorf("render pinned chart %s/%s: %w", name, version, e)
	}
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(b), 4096)
	var out []any
	for {
		var m map[string]any
		e = decoder.Decode(&m)
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, e
		}
		if len(m) == 0 {
			continue
		}
		meta, _ := m["metadata"].(map[string]any)
		annotations, _ := meta["annotations"].(map[string]any)
		if hook, _ := annotations["helm.sh/hook"].(string); hook != "" {
			continue
		}
		// Helm's release namespace does not add metadata.namespace to every resource.
		kind, _ := m["kind"].(string)
		// The pinned upstream chart always emits an agent Certificate even when
		// generateCerts=false. AEP owns leaf issuance; never let cert-manager replace it.
		labels, _ := meta["labels"].(map[string]any)
		if name == "openchoreo-data-plane" && labels["app.kubernetes.io/component"] == "cluster-agent" && (kind == "Certificate" || kind == "Issuer") {
			continue
		}
		switch kind {
		case "Namespace", "CustomResourceDefinition", "ClusterRole", "ClusterRoleBinding", "ClusterPolicy", "ValidatingWebhookConfiguration", "MutatingWebhookConfiguration", "PriorityClass", "GatewayClass", "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding":
		default:
			if meta["namespace"] == nil {
				meta["namespace"] = namespace
			}
		}
		if kind == "CustomResourceDefinition" {
			stripDescriptions(m)
		}
		restrictManifest(m)
		if e = mirrorFleetImages(m, h.Mirror); e != nil {
			return nil, e
		}
		out = append(out, m)
	}
	return out, nil
}
func checkPinnedImages(v any) error {
	switch x := v.(type) {
	case map[string]any:
		for k, v := range x {
			if k == "containers" || k == "initContainers" {
				if list, ok := v.([]any); ok {
					for _, item := range list {
						c, _ := item.(map[string]any)
						s, _ := c["image"].(string)
						if s != "" && (!strings.Contains(s, ":") || strings.HasSuffix(s, ":latest")) {
							return fmt.Errorf("unpinned image %s", s)
						}
					}
				}
			}
			if e := checkPinnedImages(v); e != nil {
				return e
			}
		}
	case []any:
		for _, v := range x {
			if e := checkPinnedImages(v); e != nil {
				return e
			}
		}
	}
	return nil
}

func restrictPod(spec map[string]any) {
	sc, _ := spec["securityContext"].(map[string]any)
	if sc == nil {
		sc = M{}
		spec["securityContext"] = sc
	}
	sc["runAsNonRoot"] = true
	sc["seccompProfile"] = M{"type": "RuntimeDefault"}
	for _, k := range []string{"containers", "initContainers"} {
		list, _ := spec[k].([]any)
		for _, v := range list {
			c, _ := v.(map[string]any)
			cs, _ := c["securityContext"].(map[string]any)
			if cs == nil {
				cs = M{}
				c["securityContext"] = cs
			}
			cs["allowPrivilegeEscalation"] = false
			cs["capabilities"] = M{"drop": []any{"ALL"}}
		}
	}
}
func restrictManifest(m M) {
	switch m["kind"] {
	case "Namespace":
		md := m["metadata"].(map[string]any)
		labels, _ := md["labels"].(map[string]any)
		if labels == nil {
			labels = M{}
			md["labels"] = labels
		}
		labels["pod-security.kubernetes.io/enforce"] = "restricted"
	case "Deployment", "StatefulSet", "DaemonSet", "Job":
		spec, _ := m["spec"].(map[string]any)
		template, _ := spec["template"].(map[string]any)
		pod, _ := template["spec"].(map[string]any)
		if pod != nil {
			restrictPod(pod)
		}
	case "Pod":
		spec, _ := m["spec"].(map[string]any)
		if spec != nil {
			restrictPod(spec)
		}
	}
}

// Documentation is not needed on the wire; retain every validation/default.
func stripDescriptions(v any) {
	switch x := v.(type) {
	case map[string]any:
		if _, ok := x["description"].(string); ok {
			delete(x, "description")
		}
		for _, v := range x {
			stripDescriptions(v)
		}
	case []any:
		for _, v := range x {
			stripDescriptions(v)
		}
	}
}

// Every concrete Pod image must be present in the embedded release inventory.
func mirrorFleetImages(v any, mirror string) error {
	switch x := v.(type) {
	case map[string]any:
		for k, value := range x {
			if k == "containers" || k == "initContainers" {
				if list, ok := value.([]any); ok {
					for _, raw := range list {
						c, _ := raw.(map[string]any)
						image, _ := c["image"].(string)
						if image != "" {
							if mirror != "" {
								image = strings.TrimPrefix(image, mirror+"/")
							}
							pin, err := inventory.Image(image)
							if err != nil {
								return err
							}
							c["image"] = registrymirror.Rewrite(pin, mirror)
						}
					}
				}
			}
			if err := mirrorFleetImages(value, mirror); err != nil {
				return err
			}
		}
	case []any:
		for _, value := range x {
			if err := mirrorFleetImages(value, mirror); err != nil {
				return err
			}
		}
	}
	return nil
}
