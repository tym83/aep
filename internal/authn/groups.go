package authn

import (
	"k8s.io/apimachinery/pkg/util/validation"
	"slices"
	"sort"
	"strings"
)

// Memberships parses canonical Keycloak groups by an explicit role suffix.
func Memberships(groups, roles []string) map[string][]string {
	result := map[string][]string{}
	for _, group := range groups {
		group = strings.TrimPrefix(group, "/")
		if !strings.HasPrefix(group, "aep-") {
			continue
		}
		group = strings.TrimPrefix(group, "aep-")
		for _, role := range roles {
			if !strings.HasSuffix(group, "-"+role) {
				continue
			}
			org := strings.TrimSuffix(group, "-"+role)
			if len(validation.IsDNS1123Label(org)) != 0 {
				continue
			}
			if !slices.Contains(result[org], role) {
				result[org] = append(result[org], role)
			}
		}
	}
	for org := range result {
		sort.Strings(result[org])
	}
	return result
}
