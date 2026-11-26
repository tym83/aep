package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/tym83/aep/internal/rcaeval"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	suite := flag.String("suite", "eval/cases.yaml", "Failure set")
	selected := flag.String("case", "", "Case ID; empty selects all")
	execute := flag.Bool("execute", false, "Mutate an isolated stand and request RCA; otherwise list cases")
	kube := flag.String("kubeconfig", "", "Stand management kubeconfig")
	recovery := flag.Bool("restore", false, "Restore the recovery journal without running a case")
	r := rcaeval.Runner{}
	flag.StringVar(&r.JournalPath, "journal", "eval-recovery.json", "Durable recovery journal; must not exist before injection")
	flag.StringVar(&r.Target.Organization, "org", "", "Organization")
	flag.StringVar(&r.Target.Tier, "tier", "nonprod", "Stand tier; prod forbidden")
	flag.StringVar(&r.Target.Quota, "quota", "", "Existing ResourceQuota name")
	flag.StringVar(&r.Target.App, "app", "", "Existing Postgres app name")
	flag.StringVar(&r.Target.WorkloadImage, "workload-image", "registry.k8s.io/pause:3.10", "Disposable quota probe image")
	flag.StringVar(&r.AgentURL, "agent-url", "", "Per-organization SRE agent base URL")
	flag.StringVar(&r.Project, "project", "", "Stand project")
	flag.StringVar(&r.Component, "component", "", "Stand component")
	flag.StringVar(&r.Environment, "environment", "dev", "Stand environment")
	flag.DurationVar(&r.Timeout, "timeout", 10*time.Minute, "Per-case deadline")
	flag.DurationVar(&r.Poll, "poll", 5*time.Second, "Report polling interval")
	flag.DurationVar(&r.Settle, "settle", 15*time.Second, "Wait for failure evidence before triggering RCA")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *execute || *recovery {
		cfg, err := clientcmd.BuildConfigFromFlags("", *kube)
		if err != nil {
			return err
		}
		r.Client, err = dynamic.NewForConfig(cfg)
		if err != nil {
			return err
		}
	}
	if *recovery {
		return rcaeval.Restore(ctx, r.Client, r.JournalPath)
	}
	set, err := rcaeval.Load(*suite)
	if err != nil {
		return err
	}
	if *execute {
		if r.Target.Organization == "" || len(validation.IsDNS1123Label(r.Target.Organization)) != 0 || r.Target.Tier == "prod" || len(validation.IsDNS1123Label(r.Target.Tier)) != 0 {
			return fmt.Errorf("valid org and non-production tier required")
		}
		r.Token = os.Getenv("AEP_EVAL_TOKEN")
		if r.AgentURL == "" || r.Project == "" || r.Component == "" || r.Token == "" {
			return fmt.Errorf("agent-url, project, component and AEP_EVAL_TOKEN required")
		}
		if !strings.HasPrefix(r.AgentURL, "https://") && !strings.HasPrefix(r.AgentURL, "http://127.0.0.1:") {
			return fmt.Errorf("agent URL must use HTTPS or a localhost port-forward")
		}
		if r.Timeout <= 0 || r.Poll <= 0 || r.Settle < 0 {
			return fmt.Errorf("invalid durations")
		}
	}
	r.Injectors = map[string]rcaeval.Injector{"quota-exhaustion": rcaeval.QuotaInjector{}, "simulated-disk": rcaeval.SimulatedDiskInjector{}}
	results := []rcaeval.Result{}
	found := false
	failed := false
	for _, c := range set.Cases {
		if *selected != "" && *selected != c.ID {
			continue
		}
		found = true
		if !*execute {
			fmt.Printf("%s\t%s\t%s\n", c.ID, c.Class, c.Injector)
			continue
		}
		out := r.Run(ctx, c)
		results = append(results, out)
		if out.Status == "failed" {
			failed = true
		}
		if out.Status == "cleanup_failed" {
			json.NewEncoder(os.Stdout).Encode(rcaeval.Summarize(results))
			return fmt.Errorf("cleanup failed; recover using --restore --journal %s", r.JournalPath)
		}
		if ctx.Err() != nil {
			break
		}
	}
	if !found {
		return fmt.Errorf("unknown case")
	}
	if *execute {
		if err = json.NewEncoder(os.Stdout).Encode(rcaeval.Summarize(results)); err != nil {
			return err
		}
	}
	if failed {
		return fmt.Errorf("one or more cases failed")
	}
	return ctx.Err()
}
