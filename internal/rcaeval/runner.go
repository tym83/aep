package rcaeval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/tym83/aep/internal/alertrouter"
	"io"
	"k8s.io/client-go/dynamic"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sigs.k8s.io/yaml"
	"strings"
	"time"
)

type Case struct {
	ID       string `json:"id"`
	Class    string `json:"class"`
	Injector string `json:"injector"`
	Inject   string `json:"inject"`
	Cleanup  string `json:"cleanup"`
	Expected string `json:"expected"`
	Match    string `json:"match"`
	Symptom  string `json:"symptom"`
}
type Suite struct {
	Cases []Case `json:"cases"`
}

func Load(path string) (Suite, error) {
	var s Suite
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	if err = yaml.UnmarshalStrict(b, &s); err != nil {
		return s, err
	}
	seen := map[string]bool{}
	counts := map[string]int{}
	for _, c := range s.Cases {
		if c.ID == "" || seen[c.ID] || c.Inject == "" || c.Cleanup == "" || c.Expected == "" || c.Symptom == "" || c.Match == "" {
			return s, fmt.Errorf("invalid or duplicate case %s", c.ID)
		}
		if _, err = regexp.Compile(c.Match); err != nil {
			return s, err
		}
		seen[c.ID] = true
		counts[c.Class]++
	}
	for _, class := range []string{"database", "volume", "node", "quota", "network", "release"} {
		if counts[class] != 5 {
			return s, fmt.Errorf("need five %s cases", class)
		}
	}
	if len(s.Cases) != 30 {
		return s, fmt.Errorf("need thirty cases")
	}
	return s, nil
}

type Result struct {
	ID        string  `json:"id"`
	Status    string  `json:"status"`
	Simulated bool    `json:"simulated"`
	Correct   bool    `json:"correct"`
	Seconds   float64 `json:"seconds"`
	ReportID  string  `json:"reportId,omitempty"`
	Error     string  `json:"error,omitempty"`
}
type Runner struct {
	Client                                                        dynamic.Interface
	Target                                                        Target
	AgentURL, Token, Project, Component, Environment, JournalPath string
	Timeout, Poll, Settle                                         time.Duration
	Injectors                                                     map[string]Injector
	HTTP                                                          *http.Client
}

func (r *Runner) Run(ctx context.Context, c Case) (out Result) {
	out.ID = c.ID
	out.Simulated = c.Injector == "simulated-disk"
	injector := r.Injectors[c.Injector]
	if injector == nil {
		out.Status = "stubbed"
		out.Error = "TODO: injector not implemented"
		return
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	out.Status = "failed"
	undo, err := injector.Inject(ctx, r.Client, r.Target, r.JournalPath)
	if undo != nil {
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if e := undo(cleanup); e != nil {
				out.Status = "cleanup_failed"
				out.Error += "; " + e.Error()
			}
		}()
	}
	if err != nil {
		out.Error = err.Error()
		return
	}
	if err = wait(ctx, r.Settle); err != nil {
		out.Error = err.Error()
		return
	}
	payload := alertrouter.AnalyzeRequest{Namespace: r.Target.Organization, Project: r.Project, Component: r.Component, Environment: r.Environment, ReleaseLine: "main", Alert: alertrouter.Alert{ID: fmt.Sprintf("eval-%d", start.UnixNano()), Value: 1, Timestamp: start.UTC().Format(time.RFC3339), Rule: map[string]any{"name": "stand-symptom", "description": c.Symptom}}}
	var accepted struct {
		ReportID string `json:"report_id"`
	}
	if err = r.request(ctx, "POST", alertrouter.Path, payload, &accepted); err != nil {
		out.Error = err.Error()
		return
	}
	if accepted.ReportID == "" {
		out.Error = "agent returned no report_id"
		return
	}
	out.ReportID = accepted.ReportID
	for {
		var report struct {
			Status string `json:"status"`
			Report struct {
				Result struct {
					RootCauses []struct {
						Summary  string `json:"summary"`
						Analysis string `json:"analysis"`
					} `json:"root_causes"`
				} `json:"result"`
			} `json:"report"`
		}
		if err = r.request(ctx, "GET", "/api/v1/rca-agent/reports/"+url.PathEscape(out.ReportID), nil, &report); err != nil {
			out.Error = err.Error()
			return
		}
		if report.Status == "completed" {
			out.Status = "completed"
			out.Seconds = time.Since(start).Seconds()
			if len(report.Report.Result.RootCauses) > 0 {
				first := report.Report.Result.RootCauses[0]
				out.Correct = regexp.MustCompile(c.Match).MatchString(first.Summary + " " + first.Analysis)
			}
			return
		}
		if report.Status == "failed" {
			out.Error = "agent analysis failed"
			return
		}
		if err = wait(ctx, r.Poll); err != nil {
			out.Error = err.Error()
			out.Seconds = time.Since(start).Seconds()
			return
		}
	}
}
func wait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (r *Runner) request(ctx context.Context, method, path string, body any, out any) error {
	var b []byte
	var err error
	if body != nil {
		b, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(r.AgentURL, "/")+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.Token)
	h := r.HTTP
	if h == nil {
		h = &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := h.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("agent HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 2<<20 {
		return fmt.Errorf("report too large")
	}
	return json.Unmarshal(data, out)
}

type Summary struct {
	Results     []Result `json:"results"`
	Eligible    int      `json:"eligible"`
	Correct     int      `json:"correct"`
	Accuracy    float64  `json:"top1Accuracy"`
	Coverage    float64  `json:"suiteCoverage"`
	MeanSeconds float64  `json:"meanTimeToReportSeconds"`
	Acceptance  bool     `json:"acceptance"`
}

func Summarize(results []Result) Summary {
	s := Summary{Results: results}
	completed := 0
	for _, r := range results {
		if r.Simulated || r.Status == "stubbed" {
			continue
		}
		s.Eligible++
		if r.Status == "completed" {
			s.MeanSeconds += r.Seconds
			completed++
			if r.Correct {
				s.Correct++
			}
		}
	}
	if s.Eligible > 0 {
		s.Accuracy = float64(s.Correct) / float64(s.Eligible)
	}
	if completed > 0 {
		s.MeanSeconds /= float64(completed)
	}
	s.Coverage = float64(s.Eligible) / 30
	s.Acceptance = s.Eligible == 30 && s.Accuracy >= 0.7
	return s
}
