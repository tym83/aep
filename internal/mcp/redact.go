package mcp

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"regexp"
	"strings"
)

var sensitive = regexp.MustCompile(`(?i)password|passwd|token|secret|credential|(^|[_-])key($|[_-])|privatekey|apikey|authorization`)

// A sensitive marker suppresses the whole free-text value, including quoted,
// whitespace-separated and multiline credentials; no guessed value boundary.
var textSensitive = regexp.MustCompile(`(?i)password|passwd|token|secret|credential|\bkey\b|api[_-]?key|authorization|bearer\s|-----BEGIN|[a-z]+://[^\s/]+:[^\s/]+@|eyJ[A-Za-z0-9_-]+\.`)
var email = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

func sanitize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		r := map[string]any{}
		for k, v := range x {
			if sensitive.MatchString(k) {
				r[k] = "[REDACTED]"
			} else {
				r[k] = sanitize(v)
			}
		}
		return r
	case []any:
		r := make([]any, len(x))
		for i, v := range x {
			r[i] = sanitize(v)
		}
		return r
	case string:
		if textSensitive.MatchString(x) {
			return "[REDACTED]"
		}
		return email.ReplaceAllString(strings.ToValidUTF8(x, ""), "[REDACTED]")
	default:
		return v
	}
}
func nestedMap(o map[string]any, p ...string) (map[string]any, bool, error) {
	return unstructured.NestedMap(o, p...)
}
