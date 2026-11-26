package itsm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	api "github.com/tym83/aep/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type ApplicationService struct {
	UID         string `json:"uid"`
	Name        string `json:"name"`
	Environment string `json:"environment"`
	Tier        string `json:"tier"`
	// Database references use namespace/kind/name, never connection secrets.
	Databases           []string `json:"databases"`
	BusinessApplication string   `json:"businessApplication,omitempty"`
}
type Poller struct {
	Client                                   client.Client
	Apps                                     dynamic.Interface
	Engine                                   *Engine
	Organization, Namespace, ConfirmationURL string
	Services                                 []ApplicationService
	SyncCMDB                                 bool
}

func (p *Poller) Once(ctx context.Context) error {
	var errs []error
	errs = append(errs, p.Engine.Retry(ctx))
	list := &api.ChangeRequestList{}
	if err := p.Client.List(ctx, list, client.InNamespace(p.Namespace)); err != nil {
		return err
	}
	for i := range list.Items {
		cr := list.Items[i]
		if cr.Spec.Organization != p.Organization {
			continue
		}
		// API resourceVersion is carried for review but only changes in spec/status
		// trigger delivery; managedFields churn must not generate ITSM work.
		cr.ManagedFields = nil
		id := p.Organization + "/" + cr.Name
		event := NewEvent("change.updated", "change/"+id, Change{ID: id, Request: &cr, ConfirmationURL: strings.TrimRight(p.ConfirmationURL, "/") + "/" + id})
		errs = append(errs, p.Engine.Submit(ctx, event))
	}
	if p.SyncCMDB {
		g, err := p.Inventory(ctx)
		if err == nil {
			err = p.Engine.Submit(ctx, NewEvent("cmdb.sync", "cmdb/"+p.Organization, g))
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
func (p *Poller) Inventory(ctx context.Context) (Graph, error) {
	org := &api.Organization{}
	if err := p.Client.Get(ctx, client.ObjectKey{Name: p.Organization}, org); err != nil {
		return Graph{}, err
	}
	g := Graph{Organization: org.Name, Epoch: org.ResourceVersion}
	root := string(org.UID)
	if root == "" {
		return g, errors.New("organization UID required")
	}
	g.Items = append(g.Items, CI{UID: root, Kind: "organization", Name: org.Name})
	clusters := map[string]string{}
	databases := map[string]string{}
	tiers := []string{}
	for tier := range org.Spec.Tiers {
		tiers = append(tiers, tier)
	}
	sort.Strings(tiers)
	for _, tier := range tiers {
		ns := "tenant-" + org.Name + "-" + tier
		for _, resource := range []struct{ Name, Kind string }{{"kuberneteses", "cluster"}, {"postgreses", "database"}, {"redises", "database"}} {
			selector := "aep.aenix.io/managed-by=bridge"
			if resource.Kind == "cluster" {
				selector = "aep.aenix.io/organization=" + root
			}
			list, err := p.Apps.Resource(schema.GroupVersionResource{Group: "apps.cozystack.io", Version: "v1alpha1", Resource: resource.Name}).Namespace(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
			if err != nil {
				return g, err
			}
			sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].GetName() < list.Items[j].GetName() })
			for _, app := range list.Items {
				uid := string(app.GetUID())
				if uid == "" {
					return g, errors.New("application UID required")
				}
				if resource.Kind == "cluster" && app.GetName() != "apps" {
					continue
				}
				g.Items = append(g.Items, CI{UID: uid, Kind: resource.Kind, Name: ns + "/" + app.GetName()})
				g.Relationships = append(g.Relationships, Relationship{Parent: root, Child: uid, Type: "Contains::Contained by"})
				if resource.Kind == "cluster" {
					clusters[tier] = uid
				} else {
					kind := "Postgres"
					if resource.Name == "redises" {
						kind = "Redis"
					}
					databases[ns+"/"+kind+"/"+app.GetName()] = uid
				}
			}
		}
	}
	business := map[string]bool{}
	for _, svc := range p.Services {
		cluster := clusters[svc.Tier]
		if cluster == "" {
			return g, fmt.Errorf("cluster missing for application service %s", svc.Name)
		}
		g.Items = append(g.Items, CI{UID: svc.UID, Kind: "application-service", Name: svc.Name + "/" + svc.Environment})
		g.Relationships = append(g.Relationships, Relationship{Parent: root, Child: svc.UID, Type: "Contains::Contained by"}, Relationship{Parent: svc.UID, Child: cluster, Type: "Runs on::Runs"})
		for _, db := range svc.Databases {
			uid := databases[db]
			if uid == "" {
				return g, fmt.Errorf("database missing: %s", db)
			}
			g.Relationships = append(g.Relationships, Relationship{Parent: svc.UID, Child: uid, Type: "Depends on::Used by"})
		}
		if svc.BusinessApplication != "" {
			uid := "business-application/" + svc.BusinessApplication
			if !business[uid] {
				g.Items = append(g.Items, CI{UID: uid, Kind: "business-application", Name: svc.Name, ExternalID: svc.BusinessApplication})
				business[uid] = true
			}
			g.Relationships = append(g.Relationships, Relationship{Parent: uid, Child: svc.UID, Type: "Consumes::Consumed by"})
		}
	}
	// Include retired CIs from the previous successful snapshot. Never infer
	// retirement after a failed or partial list.
	_, previous, err := p.Engine.Store.get(ctx, "cmdb/"+p.Organization)
	if err != nil {
		return g, err
	}
	if previous.Done && previous.Event.Type == "cmdb.sync" {
		old, err := decodeGraph(previous.Event.Data)
		if err != nil {
			return g, err
		}
		present := map[string]bool{}
		for _, ci := range g.Items {
			present[ci.UID] = true
		}
		for _, ci := range old.Items {
			if !present[ci.UID] && ci.Kind != "business-application" {
				ci.Retired = true
				g.Items = append(g.Items, ci)
			}
		}
	}
	err = ValidateGraph(g)
	return g, err
}

func decodeGraph(raw []byte) (Graph, error) {
	var g Graph
	err := json.Unmarshal(raw, &g)
	return g, err
}
