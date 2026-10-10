package monitor_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DeluxeOwl/chronicle/eventlog"
)

func TestEmbeddedAssetsHaveRestrictiveSecurityHeaders(t *testing.T) {
	t.Parallel()

	handler := newHandler(t, eventlog.NewMemory())
	assets := []struct {
		path        string
		contentType string
		contains    string
	}{
		{path: "/", contentType: "text/html; charset=utf-8", contains: `src="./assets/app.js"`},
		{
			path:        "/assets/app.js",
			contentType: "text/javascript; charset=utf-8",
			contains:    "textContent",
		},
		{
			path:        "/assets/style.css",
			contentType: "text/css; charset=utf-8",
			contains:    ":focus-visible",
		},
		{path: "/assets/favicon.svg", contentType: "image/svg+xml", contains: "<svg"},
	}
	for _, test := range assets {
		t.Run(test.path, func(t *testing.T) {
			t.Parallel()
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
			if response.Code != http.StatusOK ||
				response.Header().Get("Content-Type") != test.contentType {
				t.Fatalf(
					"status = %d, Content-Type = %s",
					response.Code,
					response.Header().Get("Content-Type"),
				)
			}
			if !strings.Contains(response.Body.String(), test.contains) {
				t.Fatalf("embedded asset %s did not contain expected content", test.path)
			}
			if response.Header().Get("X-Content-Type-Options") != "nosniff" ||
				response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("assets need nosniff and no-store")
			}
			assertContentSecurityPolicy(t, response.Header().Get("Content-Security-Policy"))
			if response.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("monitor must not grant cross-origin access")
			}
		})
	}
}

func assertContentSecurityPolicy(t *testing.T, policy string) {
	t.Helper()
	for _, directive := range []string{"default-src 'none'", "script-src 'self'", "style-src 'self'", "connect-src 'self'", "base-uri 'none'", "frame-ancestors 'none'", "form-action 'none'"} {
		if !strings.Contains(policy, directive) {
			t.Fatalf("missing CSP directive %q", directive)
		}
	}
	if strings.Contains(policy, "unsafe-inline") || strings.Contains(policy, "unsafe-eval") {
		t.Fatal("CSP must not permit inline code or eval")
	}
}

func TestMonitorMountsAtNestedPrefix(t *testing.T) {
	t.Parallel()

	handler := newHandler(t, eventlog.NewMemory())
	mux := http.NewServeMux()
	mux.Handle("/internal/tools/workflows/", http.StripPrefix("/internal/tools/workflows", handler))
	server := httptest.NewServer(mux)
	defer server.Close()

	for _, path := range []string{"/", "/assets/app.js", "/assets/style.css", "/api/workflows"} {
		request, err := http.NewRequestWithContext(
			t.Context(),
			http.MethodGet,
			server.URL+"/internal/tools/workflows"+path,
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("GET %s returned %d: %s", path, response.StatusCode, body)
		}
		if path == "/" && !strings.Contains(string(body), `href="./assets/style.css"`) {
			t.Fatal("stylesheet URL must be mount-relative")
		}
	}
}
