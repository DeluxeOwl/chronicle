package monitor_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/workflow"
)

func TestListAppliesFiltersAndPagination(t *testing.T) {
	t.Parallel()

	log := eventlog.NewMemory()
	now := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	runner := newRunner(t, log, workflow.WithNowFunc(func() time.Time { return now }))
	invoice := echoWorkflow(runner, "invoice")
	startEcho(t, invoice, "queued", false)
	now = now.Add(time.Minute)
	first := startEcho(t, invoice, "first", true)
	now = now.Add(time.Minute)
	second := startEcho(t, invoice, "second", true)
	now = now.Add(time.Minute)
	startEcho(t, echoWorkflow(runner, "notification"), "other", true)

	handler := newHandler(t, log)
	response := httptest.NewRecorder()
	handler.ServeHTTP(
		response,
		httptest.NewRequest(
			http.MethodGet,
			"/api/workflows?workflowName=invoice&status=completed&search=INVOICE&limit=1&offset=1",
			nil,
		),
	)
	page := decodePage(t, response)
	if page.Total != 2 || len(page.Instances) != 1 || page.Instances[0].InstanceID != first {
		t.Fatalf(
			"filtered page = %s, want the older of two completed invoices",
			response.Body.String(),
		)
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "/api/workflows?search="+string(second), nil),
	)
	page = decodePage(t, response)
	if page.Total != 1 || len(page.Instances) != 1 || page.Instances[0].InstanceID != second {
		t.Fatalf("ID search returned %s", response.Body.String())
	}
}

func TestListUsesBoundedDefaultPage(t *testing.T) {
	t.Parallel()

	log := eventlog.NewMemory()
	wf := echoWorkflow(newRunner(t, log), "queued")
	for range 55 {
		startEcho(t, wf, "pending", false)
	}
	handler := newHandler(t, log)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/workflows", nil))
	page := decodePage(t, response)
	if page.Total != 55 || len(page.Instances) != 50 {
		t.Fatalf(
			"default page has %d of %d instances, want 50 of 55",
			len(page.Instances),
			page.Total,
		)
	}
}

func TestDetailReturnsRecordedStepsAndEscapedPayloads(t *testing.T) {
	t.Parallel()

	log := eventlog.NewMemory()
	name := `<img src=x onerror="alert(1)">`
	payload := `</script><script>alert("private input")</script>`
	id := startEcho(t, echoWorkflow(newRunner(t, log), name), payload, true)
	handler := newHandler(t, log)
	response := httptest.NewRecorder()
	handler.ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "/api/workflows/"+string(id), nil),
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "<script") ||
		strings.Contains(response.Body.String(), "<img") {
		t.Fatal("JSON must escape HTML-sensitive characters")
	}
	var detail workflow.InstanceDetail
	if err := json.Unmarshal(response.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Summary.InstanceID != id || detail.Summary.WorkflowName != name ||
		detail.Summary.Status != workflow.StatusCompleted {
		t.Fatalf("unexpected summary: %+v", detail.Summary)
	}
	var params echoParams
	if err := json.Unmarshal(detail.Params, &params); err != nil {
		t.Fatal(err)
	}
	var output echoOutput
	if err := json.Unmarshal(detail.Output, &output); err != nil {
		t.Fatal(err)
	}
	if params.Value != payload || output.Value != payload {
		t.Fatal("HTML-sensitive input and output must round-trip without modification")
	}
	if len(detail.Steps) != 1 || detail.Steps[0].Name != "Echo input" ||
		detail.Steps[0].Status != "completed" {
		t.Fatalf("unexpected recorded path: %+v", detail.Steps)
	}
	if len(detail.History) < 3 {
		t.Fatalf("missing event history: %+v", detail.History)
	}
	for index := 1; index < len(detail.History); index++ {
		if detail.History[index].Version <= detail.History[index-1].Version {
			t.Fatal("history must retain chronological stream order")
		}
	}
}

func TestOversizedDetailReturnsCompleteErrorNotPartialJSON(t *testing.T) {
	t.Parallel()

	log := eventlog.NewMemory()
	id := startEcho(
		t,
		echoWorkflow(newRunner(t, log), "large-input"),
		strings.Repeat("a", 4<<20),
		false,
	)
	handler := newHandler(t, log)
	response := httptest.NewRecorder()
	handler.ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "/api/workflows/"+string(id), nil),
	)
	assertError(t, response, http.StatusInternalServerError)
	if response.Body.Len() > 1024 {
		t.Fatalf("oversized data escaped the response limit: %d bytes", response.Body.Len())
	}
	if strings.Contains(response.Body.String(), "large-input") ||
		strings.Contains(response.Body.String(), string(id)) {
		t.Fatal("response-limit errors must not contain partial workflow data")
	}
}

type echoParams struct {
	Value string `json:"value"`
}

type echoOutput struct {
	Value string `json:"value"`
}

func newRunner(t *testing.T, log *eventlog.Memory, opts ...workflow.RunnerOption) *workflow.Runner {
	t.Helper()
	//nolint:sloglint // The module targets Go 1.25, before slog.DiscardHandler.
	runner, err := workflow.NewRunner(log, slog.New(slog.NewTextHandler(io.Discard, nil)), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func echoWorkflow(runner *workflow.Runner, name string) *workflow.Workflow[echoParams, echoOutput] {
	return workflow.New(
		runner,
		name,
		func(wctx *workflow.Context, params *echoParams) (*echoOutput, error) {
			value, err := workflow.Step(wctx, func(context.Context) (string, error) {
				return params.Value, nil
			}, workflow.WithStepName("Echo input"))
			if err != nil {
				return nil, err
			}
			return &echoOutput{Value: value}, nil
		},
	)
}

func startEcho(
	t *testing.T,
	wf *workflow.Workflow[echoParams, echoOutput],
	value string,
	run bool,
) workflow.InstanceID {
	t.Helper()
	id, err := wf.Start(t.Context(), &echoParams{Value: value})
	if err != nil {
		t.Fatal(err)
	}
	if run {
		if _, err := wf.Run(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func decodePage(t *testing.T, response *httptest.ResponseRecorder) workflow.InstancePage {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var page workflow.InstancePage
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	return page
}
