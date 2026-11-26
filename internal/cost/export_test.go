package cost

import (
	"bytes"
	"context"
	"github.com/minio/minio-go/v7"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestS3SignedCSVUpload(t *testing.T) {
	seen := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" || r.URL.Path != "/costs/focus/2026/11/24.csv" {
			t.Errorf("unexpected upload: %s %s", r.Method, r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256") {
			t.Error("unsigned export")
		}
		body, e := io.ReadAll(r.Body)
		if e != nil || !bytes.Contains(body, []byte("BilledCost")) {
			t.Error("missing CSV")
		}
		seen = true
		w.Header().Set("ETag", `"test"`)
		w.WriteHeader(200)
	}))
	defer server.Close()
	exporter, e := NewExporter(strings.TrimPrefix(server.URL, "http://"), "costs", "test", "test-secret", "us-east-1", "test", false)
	if e != nil {
		t.Fatal(e)
	}
	var b bytes.Buffer
	if e = FocusCSV(&b, nil, hour, 24, "test"); e != nil {
		t.Fatal(e)
	}
	_, e = exporter.Client.PutObject(context.Background(), exporter.Bucket, "focus/2026/11/24.csv", bytes.NewReader(b.Bytes()), int64(b.Len()), minio.PutObjectOptions{ContentType: "text/csv"})
	if e != nil || !seen {
		t.Fatal(seen, e)
	}
}
