package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/itsm"
	"golang.org/x/oauth2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type config struct {
	CompanyID        string                    `json:"companySysId"`
	StandardTemplate string                    `json:"standardTemplate"`
	JiraEmail        string                    `json:"jiraEmail"`
	Organization     string                    `json:"organization"`
	ChangeNamespace  string                    `json:"changeNamespace"`
	StateNamespace   string                    `json:"stateNamespace"`
	Adapter          string                    `json:"adapter"`
	VendorURL        string                    `json:"vendorUrl"`
	TokenURL         string                    `json:"tokenUrl"`
	ClientID         string                    `json:"clientId"`
	AEPURL           string                    `json:"aepUrl"`
	AEPTokenURL      string                    `json:"aepTokenUrl"`
	AEPClientID      string                    `json:"aepClientId"`
	ConfirmationURL  string                    `json:"confirmationUrl"`
	SyncCMDB         bool                      `json:"syncCmdb"`
	Services         []itsm.ApplicationService `json:"services"`
	JiraProject      string                    `json:"jiraProject"`
	JiraServiceDesk  string                    `json:"jiraServiceDesk"`
	JiraRequestType  string                    `json:"jiraRequestType"`
	JiraTransitions  map[string]string         `json:"jiraTransitions"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	var kubeconfig, file, listen, cert, key string
	var interval time.Duration
	var allowHTTP bool
	flag.StringVar(&kubeconfig, "mgmt-kubeconfig", "", "Management kubeconfig; defaults to in-cluster")
	flag.StringVar(&file, "config", "/etc/itsm/config.json", "Bridge JSON configuration")
	flag.StringVar(&listen, "listen", ":8080", "HTTP listen address")
	flag.DurationVar(&interval, "poll-interval", 15*time.Second, "Change and inventory polling interval")
	flag.StringVar(&cert, "tls-cert", "", "Inbound TLS certificate")
	flag.StringVar(&key, "tls-key", "", "Inbound TLS key")
	flag.BoolVar(&allowHTTP, "allow-http", false, "Allow plaintext outbound URLs for local fake servers only")
	flag.Parse()
	if interval < time.Second {
		return errors.New("poll interval must be at least one second")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var cfg config
	if err = json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	for _, s := range []string{cfg.Organization, cfg.ChangeNamespace, cfg.StateNamespace} {
		if len(validation.IsDNS1123Label(s)) > 0 {
			return errors.New("organization and namespaces must be DNS labels")
		}
	}
	for _, s := range []string{cfg.VendorURL, cfg.AEPURL, cfg.AEPTokenURL, cfg.ConfirmationURL} {
		if err := itsm.ValidateURL(s, allowHTTP); err != nil {
			return err
		}
	}
	secret := []byte(os.Getenv("ITSM_INBOUND_HMAC_SECRET"))
	keyID := os.Getenv("ITSM_INBOUND_HMAC_KEY_ID")
	if len(secret) < 32 || keyID == "" {
		return errors.New("ITSM_INBOUND_HMAC_SECRET (32 bytes) and ITSM_INBOUND_HMAC_KEY_ID required")
	}
	if os.Getenv("AEP_CLIENT_SECRET") == "" || cfg.AEPClientID == "" {
		return errors.New("AEP client credentials required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var vendor itsm.Adapter
	switch cfg.Adapter {
	case "servicenow":
		if err := itsm.ValidateURL(cfg.TokenURL, allowHTTP); err != nil {
			return err
		}
		if cfg.ClientID == "" || os.Getenv("ITSM_CLIENT_SECRET") == "" {
			return errors.New("ServiceNow client credentials required")
		}
		vendor = &itsm.ServiceNow{StandardTemplate: cfg.StandardTemplate, HTTP: itsm.HTTP{BaseURL: cfg.VendorURL, Client: itsm.OAuthClient(ctx, cfg.TokenURL, cfg.ClientID, os.Getenv("ITSM_CLIENT_SECRET"))}}
	case "jira":
		if cfg.SyncCMDB {
			return errors.New("Jira Assets sync is not implemented; disable syncCmdb")
		}
		if cfg.JiraProject == "" || cfg.JiraServiceDesk == "" || cfg.JiraRequestType == "" || os.Getenv("ITSM_TOKEN") == "" {
			return errors.New("Jira project, service desk, request type and ITSM_TOKEN required")
		}
		hc := &http.Client{Timeout: 30 * time.Second, Transport: &oauth2.Transport{Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: os.Getenv("ITSM_TOKEN")})}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		vendorHTTP := itsm.HTTP{BaseURL: cfg.VendorURL, Client: hc}
		if cfg.JiraEmail != "" {
			vendorHTTP.BasicUser = cfg.JiraEmail
			vendorHTTP.BasicToken = os.Getenv("ITSM_TOKEN")
			vendorHTTP.Client = &http.Client{Timeout: 30 * time.Second, CheckRedirect: hc.CheckRedirect}
		}
		vendor = &itsm.Jira{HTTP: vendorHTTP, Project: cfg.JiraProject, ServiceDesk: cfg.JiraServiceDesk, RequestType: cfg.JiraRequestType, Transitions: cfg.JiraTransitions}
	case "webhook":
		outSecret := []byte(os.Getenv("ITSM_OUTBOUND_HMAC_SECRET"))
		outID := os.Getenv("ITSM_OUTBOUND_HMAC_KEY_ID")
		if len(outSecret) < 32 || outID == "" || (string(outSecret) == string(secret) || string(outSecret) == os.Getenv("ITSM_INBOUND_HMAC_PREVIOUS_SECRET")) {
			return errors.New("distinct ITSM_OUTBOUND_HMAC_SECRET (32 bytes) and ITSM_OUTBOUND_HMAC_KEY_ID required")
		}
		vendor = &itsm.Webhook{URL: cfg.VendorURL, Secret: outSecret, KeyID: outID}
	default:
		return errors.New("adapter must be servicenow, jira or webhook")
	}
	var kc *rest.Config
	if kubeconfig != "" {
		kc, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		kc, err = rest.InClusterConfig()
	}
	if err != nil {
		return err
	}
	scheme := runtime.NewScheme()
	if err = api.AddToScheme(scheme); err != nil {
		return err
	}
	if err = corev1.AddToScheme(scheme); err != nil {
		return err
	}
	cl, err := client.New(kc, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	apps, err := dynamic.NewForConfig(kc)
	if err != nil {
		return err
	}
	vendorStore := itsm.Store{Client: cl, Namespace: cfg.StateNamespace, Scope: cfg.Organization}
	engine := &itsm.Engine{Store: vendorStore, Adapter: vendor}
	noteStore := vendorStore
	noteStore.Scope += "/aep"
	notes := &itsm.Engine{Store: noteStore, Adapter: &itsm.Router{Organization: cfg.Organization, AEP: itsm.HTTP{BaseURL: cfg.AEPURL, Client: itsm.OAuthClient(ctx, cfg.AEPTokenURL, cfg.AEPClientID, os.Getenv("AEP_CLIENT_SECRET"))}}}
	keys := map[string][]byte{keyID: secret}
	if previous := os.Getenv("ITSM_INBOUND_HMAC_PREVIOUS_SECRET"); previous != "" {
		id := os.Getenv("ITSM_INBOUND_HMAC_PREVIOUS_KEY_ID")
		if len(previous) < 32 || id == "" || id == keyID {
			return errors.New("invalid previous inbound HMAC key")
		}
		keys[id] = []byte(previous)
	}

	poller := &itsm.Poller{Client: cl, Apps: apps, Engine: engine, Organization: cfg.Organization, Namespace: cfg.ChangeNamespace, ConfirmationURL: cfg.ConfirmationURL, Services: cfg.Services, SyncCMDB: cfg.SyncCMDB, CompanyID: cfg.CompanyID}
	mux := http.NewServeMux()
	mux.Handle("/v1/events", &itsm.Receiver{Engine: engine, NoteEngine: notes, Keys: keys, Organization: cfg.Organization})
	mux.Handle("GET /metrics", itsm.MetricsHandler(engine, notes))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	server := &http.Server{Addr: listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			if err := notes.Retry(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("ITSM note reconciliation: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			if err := poller.Once(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("ITSM reconciliation: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	if cert != "" || key != "" {
		err = server.ListenAndServeTLS(cert, key)
	} else {
		err = server.ListenAndServe()
	}
	if errors.Is(err, http.ErrServerClosed) {
		<-done
		return nil
	}
	return err
}
