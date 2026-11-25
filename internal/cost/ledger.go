// Package cost implements hourly provisioned-capacity accounting. OpenCost only
// redistributes VM charges; it never introduces a second set of charges.
package cost

import (
	"fmt"
	api "github.com/tym83/aep/api/v1alpha1"
	"math"
	"sort"
	"time"
)

const Domain = "aep.aenix.io/"

type Row struct {
	Organization string            `json:"organization"`
	Tier         string            `json:"tier"`
	Project      string            `json:"project"`
	Service      string            `json:"service"`
	ResourceID   string            `json:"resourceId"`
	ResourceName string            `json:"resourceName"`
	Category     string            `json:"category"`
	Currency     string            `json:"currency"`
	RateCard     string            `json:"rateCard"`
	Unallocated  bool              `json:"unallocated"`
	Tags         map[string]string `json:"tags"`
	Cost         float64           `json:"cost"`
}

func MonthHours(t time.Time) float64 {
	t = t.UTC()
	return float64(time.Date(t.Year(), t.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day() * 24)
}
func SelectRate(cards []api.RateCard, at time.Time) (*api.RateCard, error) {
	var selected *api.RateCard
	seen := map[int64]bool{}
	for i := range cards {
		c := &cards[i]
		if err := ValidateRate(c); err != nil {
			return nil, err
		}
		k := c.Spec.EffectiveFrom.Unix()
		if seen[k] {
			return nil, fmt.Errorf("duplicate effectiveFrom")
		}
		seen[k] = true
		if !c.Spec.EffectiveFrom.After(at) && (selected == nil || c.Spec.EffectiveFrom.After(selected.Spec.EffectiveFrom.Time)) {
			selected = c
		}
	}
	if selected == nil {
		return nil, fmt.Errorf("no rate card effective at %s", at)
	}
	return selected, nil
}
func ValidateRate(c *api.RateCard) error {
	if len(c.Spec.Currency) != 3 || c.Spec.EffectiveFrom.IsZero() || !c.Spec.EffectiveFrom.Time.Equal(c.Spec.EffectiveFrom.Truncate(time.Hour)) {
		return fmt.Errorf("invalid currency or effective hour in %s", c.Name)
	}
	rates := []float64{c.Spec.VCPUHour, c.Spec.MemoryGiBHour, c.Spec.ExternalIPMonth, c.Spec.BucketGiBMonth}
	for _, v := range c.Spec.StorageGiBMonth {
		rates = append(rates, v)
	}
	for _, v := range c.Spec.GPUHour {
		rates = append(rates, v)
	}
	for key, v := range c.Spec.Shared {
		if key != "management" && key != "aep-cp" && key != "opensearch" && key != "harbor" {
			return fmt.Errorf("unknown shared category %s", key)
		}
		if v.AllocationKey != "equal" && v.AllocationKey != "resourceShare" {
			return fmt.Errorf("invalid allocation key")
		}
		rates = append(rates, v.Monthly)
	}
	for _, v := range rates {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("invalid negative or nonfinite rate")
		}
	}
	return nil
}

// AllocateShared uses direct provisioned cost as the resource-share weight.
// Zero-demand organizations still participate in equal allocation. No orgs is
// an error: platform expenses must not silently disappear.
func AllocateShared(rows []Row, orgs []string, c *api.RateCard, at time.Time) ([]Row, error) {
	weights := map[string]float64{}
	total := 0.0
	for _, r := range rows {
		weights[r.Organization] += r.Cost
		total += r.Cost
	}
	if len(orgs) == 0 && len(c.Spec.Shared) > 0 {
		return nil, fmt.Errorf("cannot allocate shared costs without organizations")
	}
	sort.Strings(orgs)
	for name, shared := range c.Spec.Shared {
		for _, org := range orgs {
			w := 1 / float64(len(orgs))
			if shared.AllocationKey == "resourceShare" && total > 0 {
				w = weights[org] / total
			}
			rows = append(rows, Row{Organization: org, Tier: "shared", Service: name, ResourceID: "platform/" + name, ResourceName: name, Category: "Shared", Currency: c.Spec.Currency, RateCard: c.Name, Tags: map[string]string{Domain + "organization": org}, Cost: shared.Monthly / MonthHours(at) * w})
		}
	}
	return rows, nil
}
