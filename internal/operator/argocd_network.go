package operator

import (
	"context"
	"fmt"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"net"
	"net/url"
	"sort"
	"strconv"
)

type hostResolver func(context.Context, string) ([]net.IPAddr, error)

// Standard NetworkPolicy has no FQDN peers. Refresh exact host addresses each
// reconciliation; a DNS failure prevents publishing a broader policy.
func argoEgress(ctx context.Context, vcsURL, keycloakURL string, resolve hostResolver) (*unstructured.Unstructured, error) {
	egress := []any{M{"to": []any{M{"namespaceSelector": M{"matchLabels": M{"kubernetes.io/metadata.name": "kube-system"}}, "podSelector": M{"matchLabels": M{"k8s-app": "kube-dns"}}}}, "ports": []any{M{"protocol": "UDP", "port": int64(53)}, M{"protocol": "TCP", "port": int64(53)}}},
		// Repo-server requires its local Redis cache.
		M{"to": []any{M{"podSelector": M{"matchLabels": M{"app.kubernetes.io/name": "argocd-redis"}}}}, "ports": []any{M{"protocol": "TCP", "port": int64(6379)}}}}
	for _, raw := range []string{vcsURL, keycloakURL} {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" {
			return nil, fmt.Errorf("egress endpoint must be HTTPS")
		}
		port := int64(443)
		if u.Port() != "" {
			port, err = strconv.ParseInt(u.Port(), 10, 32)
			if err != nil || port < 1 || port > 65535 {
				return nil, fmt.Errorf("invalid egress port")
			}
		}
		ips, err := resolve(ctx, u.Hostname())
		if err != nil || len(ips) == 0 {
			return nil, fmt.Errorf("cannot resolve allowed egress host %s", u.Hostname())
		}
		cidrs := map[string]bool{}
		for _, addr := range ips {
			if addr.IP == nil {
				continue
			}
			suffix := "/128"
			if addr.IP.To4() != nil {
				suffix = "/32"
			}
			cidrs[addr.IP.String()+suffix] = true
		}
		names := []string{}
		for cidr := range cidrs {
			names = append(names, cidr)
		}
		sort.Strings(names)
		peers := []any{}
		for _, cidr := range names {
			peers = append(peers, M{"ipBlock": M{"cidr": cidr}})
		}
		if len(peers) == 0 {
			return nil, fmt.Errorf("no egress addresses")
		}
		egress = append(egress, M{"to": peers, "ports": []any{M{"protocol": "TCP", "port": port}}})
	}
	return obj("networking.k8s.io/v1", "NetworkPolicy", "argocd", "aep-repo-server-egress", M{"podSelector": M{"matchLabels": M{"app.kubernetes.io/name": "argocd-repo-server"}}, "policyTypes": []any{"Egress"}, "egress": egress}), nil
}
