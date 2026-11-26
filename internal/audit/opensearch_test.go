package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestOpenSearchPoisonIsolation(t *testing.T) {
	for _, status := range []int{400, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			store := &capturedStore{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"errors":true,"items":[{"index":{"status":201}},{"index":{"status":%d,"error":{"type":"mapper_parsing_exception","reason":"bad field"}}}]}`, status)
			}))
			defer srv.Close()
			s := openTest(t)
			if err := s.Append([]Event{{Action: "ok"}, {Action: "bad"}}); err != nil {
				t.Fatal(err)
			}
			archive := &objectSink{}
			err := s.Flush(context.Background(), []Sink{OpenSearch{URL: srv.URL, Index: "test", DeadLetters: store}, S3{Store: archive}})
			if len(archive.keys) == 0 {
				t.Fatal("bad document blocked other sink")
			}
			if status == 400 {
				if err != nil || len(s.Pending()) != 0 || len(store.keys) != 1 {
					t.Fatal("poison document not isolated", err)
				}
			} else if err == nil || len(s.Pending()) != 2 || len(store.keys) != 0 {
				t.Fatal("retryable error discarded")
			}
		})
	}
}
func TestOpenSearchTemplateDisablesDetailsMapping(t *testing.T) {
	b, err := os.ReadFile("../../config/audit/opensearch-index-template.json")
	if err != nil {
		t.Fatal(err)
	}
	var template struct {
		Template struct {
			Mappings struct{ Properties map[string]map[string]any }
		}
	}
	if err = json.Unmarshal(b, &template); err != nil {
		t.Fatal(err)
	}
	p := template.Template.Mappings.Properties
	if p["details"]["enabled"] != false || p["detailsRaw"]["index"] != false {
		t.Fatal("heterogeneous evidence can cause mapping conflicts")
	}
}
