package monitor_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/workflow"
	"github.com/DeluxeOwl/chronicle/workflow/monitor"
)

func TestNewHandlerRejectsNilInspector(t *testing.T) {
	t.Parallel()

	handler, err := monitor.NewHandler(nil)
	if err == nil || handler != nil {
		t.Fatalf("NewHandler(nil) = (%v, %v), want nil handler and an error", handler, err)
	}
}

func TestEmptyWorkflowList(t *testing.T) {
	t.Parallel()

	handler := newHandler(t, eventlog.NewMemory())
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/workflows", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var page struct {
		Instances []json.RawMessage `json:"instances"`
		Total     int               `json:"total"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Instances == nil || len(page.Instances) != 0 || page.Total != 0 {
		t.Fatalf("empty page = %s, want an empty array and total 0", response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("workflow data must not be cached")
	}
	if response.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("unexpected Content-Type: %s", response.Header().Get("Content-Type"))
	}
}

func assertError(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d, body = %s", response.Code, status, response.Body.String())
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("error response is not valid JSON: %v", err)
	}
	if body.Error == "" {
		t.Fatal("error response must contain a message")
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("error responses must not be cached")
	}
	if response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("error responses need nosniff")
	}
	if strings.Contains(response.Body.String(), "<script") {
		t.Fatal("error response contains raw HTML")
	}
}

func newHandler(t *testing.T, log event.GlobalLog) http.Handler {
	t.Helper()
	inspector, err := workflow.NewInspector(log)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := monitor.NewHandler(inspector)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}
