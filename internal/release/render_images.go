package release

import (
	"encoding/json"
	"github.com/tym83/aep/internal/registrymirror"
	inventory "github.com/tym83/aep/release"
	"io"
	kyaml "k8s.io/apimachinery/pkg/util/yaml"
	"strings"
)

// RenderImages is a Helm post-renderer for third-party charts with heterogeneous
// image settings. Unknown images fail closed instead of escaping the mirror.
func RenderImages(in io.Reader, out io.Writer, mirror string) error {
	dec := kyaml.NewYAMLOrJSONDecoder(in, 4096)
	for {
		var m map[string]any
		err := dec.Decode(&m)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if len(m) == 0 {
			continue
		}
		if err = pinRenderedImages(m, mirror); err != nil {
			return err
		}
		if _, err = io.WriteString(out, "---\n"); err != nil {
			return err
		}
		if err = json.NewEncoder(out).Encode(m); err != nil {
			return err
		}
	}
}
func pinRenderedImages(v any, mirror string) error {
	switch x := v.(type) {
	case map[string]any:
		for k, value := range x {
			if k == "containers" || k == "initContainers" || k == "ephemeralContainers" {
				if list, ok := value.([]any); ok {
					for _, raw := range list {
						c, _ := raw.(map[string]any)
						ref, _ := c["image"].(string)
						if ref == "" {
							continue
						}
						if mirror != "" {
							ref = strings.TrimPrefix(ref, mirror+"/")
						}
						pin, err := inventory.Image(ref)
						if err != nil {
							return err
						}
						c["image"] = registrymirror.Rewrite(pin, mirror)
					}
				}
			}
			if err := pinRenderedImages(value, mirror); err != nil {
				return err
			}
		}
	case []any:
		for _, value := range x {
			if err := pinRenderedImages(value, mirror); err != nil {
				return err
			}
		}
	}
	return nil
}
