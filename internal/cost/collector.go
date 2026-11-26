package cost

import (
	"context"
	"fmt"
	api "github.com/tym83/aep/api/v1alpha1"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"strings"
	"time"
)

type Collector struct {
	Client   dynamic.Interface
	OpenCost *OpenCost
}

func GVR(group, version, name string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: group, Version: version, Resource: name}
}
func (c *Collector) list(ctx context.Context, g, version, r, ns string) ([]unstructured.Unstructured, error) {
	l, e := c.Client.Resource(GVR(g, version, r)).Namespace(ns).List(ctx, metav1.ListOptions{})
	if e != nil {
		return nil, e
	}
	return l.Items, nil
}
func str(u *unstructured.Unstructured, p ...string) string {
	v, _, _ := unstructured.NestedString(u.Object, p...)
	return v
}
func number(u *unstructured.Unstructured, p ...string) float64 {
	v, _, _ := unstructured.NestedInt64(u.Object, p...)
	return float64(v)
}
func quantity(s string) (float64, error) {
	q, e := resource.ParseQuantity(s)
	if e != nil {
		return 0, e
	}
	return q.AsApproximateFloat64(), nil
}
func (c *Collector) VMSize(ctx context.Context, vm *unstructured.Unstructured) (float64, float64, error) {
	if name := str(vm, "spec", "instancetype", "name"); name != "" {
		kind := strings.ToLower(str(vm, "spec", "instancetype", "kind"))
		ns := ""
		r := "virtualmachineclusterinstancetypes"
		if kind == "virtualmachineinstancetype" {
			ns = vm.GetNamespace()
			r = "virtualmachineinstancetypes"
		}
		it, e := c.Client.Resource(GVR("instancetype.kubevirt.io", "v1beta1", r)).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if e != nil {
			return 0, 0, e
		}
		mem, e := quantity(str(it, "spec", "memory", "guest"))
		return number(it, "spec", "cpu", "guest"), mem / (1 << 30), e
	}
	cpu := 1.0
	for _, k := range []string{"cores", "sockets", "threads"} {
		v := number(vm, "spec", "template", "spec", "domain", "cpu", k)
		if v > 0 {
			cpu *= v
		}
	}
	mem := str(vm, "spec", "template", "spec", "domain", "memory", "guest")
	if mem == "" {
		mem = str(vm, "spec", "template", "spec", "domain", "resources", "requests", "memory")
	}
	m, e := quantity(mem)
	return cpu, m / (1 << 30), e
}

// PodRequests follows scheduling requests, including restartable init containers
// and overhead; terminal pods and virt-launcher are excluded by Collect.
func PodRequests(p *core.Pod) (float64, float64) {
	sum := func(rl core.ResourceList) (float64, float64) {
		return rl.Cpu().AsApproximateFloat64(), rl.Memory().AsApproximateFloat64() / (1 << 30)
	}
	cpu, mem := 0.0, 0.0
	for _, v := range p.Spec.Containers {
		a, b := sum(v.Resources.Requests)
		cpu += a
		mem += b
	}
	sideCPU, sideMem, maxCPU, maxMem := 0.0, 0.0, 0.0, 0.0
	for _, v := range p.Spec.InitContainers {
		a, b := sum(v.Resources.Requests)
		if v.RestartPolicy != nil && *v.RestartPolicy == core.ContainerRestartPolicyAlways {
			sideCPU += a
			sideMem += b
			a = 0
			b = 0
		}
		maxCPU = max(maxCPU, a+sideCPU)
		maxMem = max(maxMem, b+sideMem)
	}
	cpu = max(cpu+sideCPU, maxCPU)
	mem = max(mem+sideMem, maxMem)
	if p.Spec.Resources != nil {
		if q, ok := p.Spec.Resources.Requests[core.ResourceCPU]; ok {
			cpu = q.AsApproximateFloat64()
		}
		if q, ok := p.Spec.Resources.Requests[core.ResourceMemory]; ok {
			mem = q.AsApproximateFloat64() / (1 << 30)
		}
	}
	a, b := sum(p.Spec.Overhead)
	return cpu + a, mem + b
}
func baseRow(org, tier, category string, u *unstructured.Unstructured, card *api.RateCard, apps []unstructured.Unstructured) Row {
	tags := map[string]string{}
	for k, v := range u.GetLabels() {
		if strings.HasPrefix(k, Domain) {
			tags[k] = v
		}
	}
	// Helm release identity joins workload labels to the app's authoritative AEP labels.
	for _, app := range apps {
		release := strings.ToLower(app.GetKind()) + "-" + app.GetName()
		l := u.GetLabels()
		match := l["helm.toolkit.fluxcd.io/name"] == release || l["app.kubernetes.io/instance"] == release || l["app.kubernetes.io/instance"] == app.GetName() || l["cnpg.io/cluster"] == app.GetName() || l["cnpg.io/cluster"] == release || l["redisfailovers.databases.spotahome.com/name"] == release
		if match {
			for k, v := range app.GetLabels() {
				if strings.HasPrefix(k, Domain) {
					tags[k] = v
				}
			}
			break
		}
	}
	tags[Domain+"organization"] = org
	tags[Domain+"tier"] = tier
	service := tags[Domain+"resource"]
	if service == "" {
		service = tags[Domain+"component"]
	}
	return Row{Organization: org, Tier: tier, Project: tags[Domain+"project"], Service: service, ResourceID: u.GetNamespace() + "/" + u.GetKind() + "/" + u.GetName(), ResourceName: u.GetName(), Category: category, Currency: card.Spec.Currency, RateCard: card.Name, Tags: tags}
}
func (c *Collector) Collect(ctx context.Context, at time.Time) ([]Row, error) {
	raw, e := c.list(ctx, "aep.aenix.io", "v1alpha1", "ratecards", "")
	if e != nil {
		return nil, e
	}
	cards := []api.RateCard{}
	for _, u := range raw {
		var card api.RateCard
		if e = runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &card); e != nil {
			return nil, e
		}
		cards = append(cards, card)
	}
	card, e := SelectRate(cards, at)
	if e != nil {
		return nil, e
	}
	organizations, e := c.list(ctx, "aep.aenix.io", "v1alpha1", "organizations", "")
	if e != nil {
		return nil, e
	}
	rows := []Row{}
	orgs := []string{}
	for _, o := range organizations {
		org := o.GetName()
		orgs = append(orgs, org)
		tiers, _, _ := unstructured.NestedMap(o.Object, "spec", "tiers")
		for tier := range tiers {
			ns := "tenant-" + org + "-" + tier
			apps := []unstructured.Unstructured{}
			for _, r := range []string{"postgreses", "redises", "kuberneteses", "buckets"} {
				items, err := c.list(ctx, "apps.cozystack.io", "v1alpha1", r, ns)
				if err != nil {
					return nil, err
				}
				apps = append(apps, items...)
			}
			vms, err := c.list(ctx, "kubevirt.io", "v1", "virtualmachines", ns)
			if err != nil {
				return nil, err
			}
			vmRows := []Row{}
			for i := range vms {
				vm := &vms[i]
				cpu, mem, err := c.VMSize(ctx, vm)
				if err != nil {
					return nil, fmt.Errorf("VM %s: %w", vm.GetName(), err)
				}
				row := baseRow(org, tier, "Compute", vm, card, apps)
				row.Cost = cpu*card.Spec.VCPUHour + mem*card.Spec.MemoryGiBHour
				devices, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "domain", "devices", "gpus")
				for _, v := range devices {
					m, ok := v.(map[string]any)
					if !ok {
						return nil, fmt.Errorf("invalid GPU spec")
					}
					model, _ := m["deviceName"].(string)
					rate, ok := card.Spec.GPUHour[model]
					if !ok {
						return nil, fmt.Errorf("missing GPU rate %s", model)
					}
					row.Cost += rate
				}
				vmRows = append(vmRows, row)
			}
			if c.OpenCost != nil {
				vmRows = c.OpenCost.Split(ctx, org, tier, at, vmRows, card)
			} else {
				for i := range vmRows {
					vmRows[i].Unallocated = true
					vmRows[i].Project = ""
					vmRows[i].Service = ""
				}
			}
			rows = append(rows, vmRows...)
			pods, err := c.list(ctx, "", "v1", "pods", ns)
			if err != nil {
				return nil, err
			}
			for i := range pods {
				u := &pods[i]
				var p core.Pod
				if err = runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &p); err != nil {
					return nil, err
				}
				if p.Labels["kubevirt.io"] == "virt-launcher" || p.Labels["kubevirt.io/created-by"] != "" || p.Status.Phase == core.PodSucceeded || p.Status.Phase == core.PodFailed {
					continue
				}
				a, b := PodRequests(&p)
				row := baseRow(org, tier, "Compute", u, card, apps)
				row.Cost = a*card.Spec.VCPUHour + b*card.Spec.MemoryGiBHour
				row.Unallocated = row.Service == ""
				rows = append(rows, row)
			}
			pvcs, err := c.list(ctx, "", "v1", "persistentvolumeclaims", ns)
			if err != nil {
				return nil, err
			}
			for i := range pvcs {
				u := &pvcs[i]
				size, err := quantity(str(u, "spec", "resources", "requests", "storage"))
				if err != nil {
					return nil, err
				}
				class := str(u, "spec", "storageClassName")
				rate, ok := card.Spec.StorageGiBMonth[class]
				if !ok {
					return nil, fmt.Errorf("missing storage rate for %q", class)
				}
				row := baseRow(org, tier, "Storage", u, card, apps)
				row.Cost = size / (1 << 30) * rate / MonthHours(at)
				row.Unallocated = row.Service == ""
				rows = append(rows, row)
			}
			services, err := c.list(ctx, "", "v1", "services", ns)
			if err != nil {
				return nil, err
			}
			seen := map[string]bool{}
			for i := range services {
				u := &services[i]
				ingress, _, _ := unstructured.NestedSlice(u.Object, "status", "loadBalancer", "ingress")
				for _, v := range ingress {
					m, _ := v.(map[string]any)
					ip, _ := m["ip"].(string)
					if ip == "" || seen[ip] {
						continue
					}
					seen[ip] = true
					row := baseRow(org, tier, "Networking", u, card, apps)
					row.ResourceID += "/" + ip
					row.Cost = card.Spec.ExternalIPMonth / MonthHours(at)
					rows = append(rows, row)
				}
			}
			// Bucket used bytes are explicitly supplied by the storage metering integration;
			// never substitute a quota for usage or silently emit a zero charge.
			for i := range apps {
				u := &apps[i]
				if u.GetKind() != "Bucket" {
					continue
				}
				bytes := u.GetAnnotations()[Domain+"bucket-used-bytes"]
				if bytes == "" {
					return nil, fmt.Errorf("bucket %s has no metered bucket-used-bytes", u.GetName())
				}
				size, err := quantity(bytes)
				if err != nil {
					return nil, err
				}
				row := baseRow(org, tier, "Storage", u, card, nil)
				row.Cost = size / (1 << 30) * card.Spec.BucketGiBMonth / MonthHours(at)
				rows = append(rows, row)
			}
		}
	}
	return AllocateShared(rows, orgs, card, at)
}
