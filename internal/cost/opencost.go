package cost

import (
	"context"
	"encoding/json"
	api "github.com/tym83/aep/api/v1alpha1"
	"io"
	"maps"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type OpenCost struct {
	URLTemplate string
	Client      *http.Client
}
type allocation struct {
	CPU        float64 `json:"cpuCoreRequestAverage"`
	RAM        float64 `json:"ramByteRequestAverage"`
	Minutes    float64 `json:"minutes"`
	Properties struct {
		Labels map[string]string `json:"labels"`
	} `json:"properties"`
}

func (o *OpenCost) Split(ctx context.Context, org, tier string, at time.Time, rows []Row, card *api.RateCard) []Row {
	fallback := func() []Row {
		for i := range rows {
			rows[i].Unallocated = true
			rows[i].Project = ""
			rows[i].Service = ""
		}
		return rows
	}
	raw := strings.NewReplacer("{org}", url.PathEscape(org), "{tier}", url.PathEscape(tier)).Replace(o.URLTemplate)
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fallback()
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/allocation"
	q := u.Query()
	q.Set("window", at.UTC().Format(time.RFC3339)+","+at.Add(time.Hour).UTC().Format(time.RFC3339))
	q.Set("aggregate", "pod")
	q.Set("includeIdle", "false")
	q.Set("accumulate", "true")
	u.RawQuery = q.Encode()
	req, e := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if e != nil {
		return fallback()
	}
	cl := o.Client
	if cl == nil {
		cl = &http.Client{Timeout: 15 * time.Second}
	}
	resp, e := cl.Do(req)
	if e != nil {
		return fallback()
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fallback()
	}
	var result struct {
		Code int                     `json:"code"`
		Data []map[string]allocation `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&result) != nil || result.Code != 200 || len(result.Data) != 1 {
		return fallback()
	}
	type part struct {
		project, component string
		weight             float64
	}
	parts := []part{}
	total := 0.0
	for _, a := range result.Data[0] {
		if a.CPU < 0 || a.RAM < 0 || a.Minutes <= 0 || a.Minutes > 60 || math.IsNaN(a.CPU) || math.IsNaN(a.RAM) {
			return fallback()
		}
		w := (a.CPU*card.Spec.VCPUHour + a.RAM/(1<<30)*card.Spec.MemoryGiBHour) * a.Minutes / 60
		if w == 0 {
			continue
		}
		l := a.Properties.Labels
		label := func(keys ...string) string {
			for _, key := range keys {
				if l[key] != "" {
					return l[key]
				}
				sanitized := strings.NewReplacer("/", "_", ".", "_", "-", "_").Replace(key)
				if l[sanitized] != "" {
					return l[sanitized]
				}
			}
			return ""
		}
		project := label(Domain+"project", "openchoreo.dev/project", "openchoreo.dev/project-uid")
		component := label(Domain+"component", "openchoreo.dev/component", "openchoreo.dev/component-uid")
		parts = append(parts, part{project, component, w})
		total += w
	}
	if total <= 0 {
		return fallback()
	}
	// Demand below provisioned value leaves idle; overcommitted demand is normalized.
	vmTotal := 0.0
	for _, r := range rows {
		vmTotal += r.Cost
	}
	denom := max(total, vmTotal)
	if denom == 0 {
		return rows
	}
	out := []Row{}
	for _, r := range rows {
		for _, p := range parts {
			v := r
			v.Project = p.project
			v.Service = p.component
			v.Unallocated = p.project == "" || p.component == ""
			v.Cost = r.Cost * p.weight / denom
			v.Tags = maps.Clone(r.Tags)
			v.Tags[Domain+"project"] = p.project
			v.Tags[Domain+"component"] = p.component
			out = append(out, v)
		}
		if total < denom {
			v := r
			v.Service = "idle"
			v.Project = ""
			v.Cost = r.Cost * (1 - total/denom)
			out = append(out, v)
		}
	}
	return out
}
