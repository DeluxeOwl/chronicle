package monitor_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/version"
)

func TestListRejectsInvalidQueryBeforeReadingHistory(t *testing.T) {
	t.Parallel()

	handler := newHandler(t, failingLog{Memory: eventlog.NewMemory()})
	queries := []string{
		"limit=0", "limit=201", "limit=-1", "limit=", "limit=1.5", "limit=%2B2", "limit=%202",
		"limit=999999999999999999999999", "offset=-1", "offset=", "offset=1.5", "offset=%2B1",
		"offset=999999999999999999999999", "status=unknown", "status=Running", "status=failed&status=running",
		"search=one&search=two", "workflowName=one&workflowName=two", "limit=1&limit=2", "offset=0&offset=1",
		"unknown=value", "search=%00", "workflowName=%0A", "search=%FF", "search=%zz", "search=one;status=failed",
		"search=" + strings.Repeat("a", 257), "workflowName=" + strings.Repeat("a", 257),
		"search=" + strings.Repeat("a", 4097),
	}
	for _, query := range queries {
		t.Run(query[:min(len(query), 80)], func(t *testing.T) {
			t.Parallel()
			request := httptest.NewRequest(http.MethodGet, "/api/workflows", nil)
			request.URL.RawQuery = query
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			assertError(t, response, http.StatusBadRequest)
		})
	}
}

func TestListAcceptsBoundariesAndDeferredStatuses(t *testing.T) {
	t.Parallel()

	handler := newHandler(t, eventlog.NewMemory())
	queries := []string{
		"limit=1&offset=0", "limit=200&offset=999999", "workflowName=&status=&search=",
		"search=" + strings.Repeat(
			"a",
			256,
		), "workflowName=" + url.QueryEscape(strings.Repeat("é", 128)),
		"status=running", "status=sleeping", "status=waiting", "status=retrying", "status=completed", "status=failed", "status=cancelled",
	}
	for _, query := range queries {
		t.Run(query[:min(len(query), 80)], func(t *testing.T) {
			t.Parallel()
			response := httptest.NewRecorder()
			handler.ServeHTTP(
				response,
				httptest.NewRequest(http.MethodGet, "/api/workflows?"+query, nil),
			)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestReadOnlyRoutesAndPathValidation(t *testing.T) {
	t.Parallel()

	handler := newHandler(t, eventlog.NewMemory())
	tests := []struct {
		method string
		path   string
		status int
	}{
		{method: http.MethodPost, path: "/", status: http.StatusMethodNotAllowed},
		{method: http.MethodHead, path: "/assets/app.js", status: http.StatusMethodNotAllowed},
		{method: http.MethodPut, path: "/assets/style.css", status: http.StatusMethodNotAllowed},
		{method: http.MethodPost, path: "/api/workflows", status: http.StatusMethodNotAllowed},
		{
			method: http.MethodDelete,
			path:   "/api/workflows/missing",
			status: http.StatusMethodNotAllowed,
		},
		{method: http.MethodOptions, path: "/api/workflows", status: http.StatusMethodNotAllowed},
		{method: http.MethodGet, path: "/api/workflows/missing", status: http.StatusNotFound},
		{method: http.MethodGet, path: "/api/workflows/", status: http.StatusBadRequest},
		{method: http.MethodGet, path: "/api/workflows/%00", status: http.StatusBadRequest},
		{method: http.MethodGet, path: "/api/workflows/%FF", status: http.StatusBadRequest},
		{method: http.MethodGet, path: "/api/workflows/..", status: http.StatusBadRequest},
		{method: http.MethodGet, path: "/api/workflows/%5C", status: http.StatusBadRequest},
		{
			method: http.MethodGet,
			path:   "/api/workflows/" + strings.Repeat("a", 513),
			status: http.StatusBadRequest,
		},
		{
			method: http.MethodGet,
			path:   "/api/workflows/missing?search=abc",
			status: http.StatusBadRequest,
		},
		{
			method: http.MethodGet,
			path:   "/api/workflows/missing/cancel",
			status: http.StatusNotFound,
		},
		{method: http.MethodGet, path: "/api/workflows/a%2Fb", status: http.StatusNotFound},
		{method: http.MethodGet, path: "/assets/", status: http.StatusNotFound},
		{method: http.MethodGet, path: "/assets/index.html", status: http.StatusNotFound},
		{method: http.MethodGet, path: "/assets/../handler.go", status: http.StatusNotFound},
		{method: http.MethodGet, path: "/missing", status: http.StatusNotFound},
	}
	for _, test := range tests {
		t.Run(test.method+" "+test.path[:min(len(test.path), 80)], func(t *testing.T) {
			t.Parallel()
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
			assertError(t, response, test.status)
			if test.status == http.StatusMethodNotAllowed &&
				response.Header().Get("Allow") != http.MethodGet {
				t.Fatal("405 response must advertise GET in Allow")
			}
		})
	}
}

func TestBackendFailuresDoNotExposeInternalErrors(t *testing.T) {
	t.Parallel()

	handler := newHandler(t, failingLog{Memory: eventlog.NewMemory()})
	for _, path := range []string{"/api/workflows", "/api/workflows/existing"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
			assertError(t, response, http.StatusInternalServerError)
			if strings.Contains(response.Body.String(), "private database password") {
				t.Fatal("backend errors must not be exposed to clients")
			}
		})
	}
}

type failingLog struct {
	*eventlog.Memory
}

func (failingLog) ReadEvents(context.Context, event.LogID, version.Selector) event.Records {
	return func(yield func(*event.Record, error) bool) {
		yield(nil, errors.New("private database password"))
	}
}

func (failingLog) ReadAllEvents(context.Context, version.Selector) event.GlobalRecords {
	return func(yield func(*event.GlobalRecord, error) bool) {
		yield(nil, errors.New("private database password"))
	}
}
