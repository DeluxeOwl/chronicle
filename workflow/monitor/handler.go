package monitor

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/workflow"
)

const (
	maxQueryBytes      = 4096
	maxFilterBytes     = 256
	maxInstanceIDBytes = 512
	maxResponseBytes   = 4 << 20
	queryTimeout       = 10 * time.Second
	defaultLimit       = 50
	maxLimit           = 200

	contentTypeJSON = "application/json; charset=utf-8"
	contentTypeHTML = "text/html; charset=utf-8"
	contentTypeJS   = "text/javascript; charset=utf-8"
	contentTypeCSS  = "text/css; charset=utf-8"
	contentTypeSVG  = "image/svg+xml"
)

//go:embed assets/index.html assets/app.js assets/style.css assets/favicon.svg
var embeddedAssets embed.FS

type asset struct {
	body        []byte
	contentType string
}

type handler struct {
	inspector *workflow.Inspector
	assets    map[string]asset
}

// NewHandler returns a read-only workflow monitor. The host must protect both
// the page and its API with authentication and authorization middleware.
// Workflow parameters, results, errors, and history can contain sensitive data.
func NewHandler(inspector *workflow.Inspector) (http.Handler, error) {
	if inspector == nil {
		return nil, errors.New("monitor inspector cannot be nil")
	}

	assets := make(map[string]asset, 4)
	for path, contentType := range map[string]string{
		"/":                   contentTypeHTML,
		"/assets/app.js":      contentTypeJS,
		"/assets/style.css":   contentTypeCSS,
		"/assets/favicon.svg": contentTypeSVG,
	} {
		filename := strings.TrimPrefix(path, "/")
		if path == "/" {
			filename = "assets/index.html"
		}
		body, err := embeddedAssets.ReadFile(filename)
		if err != nil {
			return nil, fmt.Errorf("read monitor asset %s: %w", filename, err)
		}
		assets[path] = asset{body: body, contentType: contentType}
	}

	return &handler{inspector: inspector, assets: assets}, nil
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	static, isAsset := h.assets[r.URL.Path]
	isList := r.URL.Path == "/api/workflows"
	isDetail := strings.HasPrefix(r.URL.Path, "/api/workflows/")
	if !isAsset && !isList && !isDetail {
		writeError(w, http.StatusNotFound, "Resource not found.")
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "Only GET requests are supported.")
		return
	}
	if isAsset {
		writeBody(w, http.StatusOK, static.contentType, static.body)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	if isList {
		h.list(w, r, ctx)
		return
	}
	h.detail(w, r, ctx)
}

func (h *handler) list(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	opts, err := parseListOptions(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	page, err := h.inspector.List(ctx, opts)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Unable to load workflows.")
		return
	}
	writeJSON(w, page)
}

func (h *handler) detail(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	id := strings.TrimPrefix(r.URL.Path, "/api/workflows/")
	if strings.Contains(id, "/") {
		writeError(w, http.StatusNotFound, "Resource not found.")
		return
	}
	if !validText(id, maxInstanceIDBytes) || id == "" || id == "." || id == ".." ||
		strings.Contains(id, "\\") {
		writeError(w, http.StatusBadRequest, "The workflow instance ID is invalid.")
		return
	}
	if r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "Workflow details do not accept query parameters.")
		return
	}

	detail, err := h.inspector.Get(ctx, workflow.InstanceID(id))
	if errors.Is(err, aggregate.ErrRootNotFound) {
		writeError(w, http.StatusNotFound, "Workflow not found.")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Unable to load workflow details.")
		return
	}
	writeJSON(w, detail)
}

func parseListOptions(rawQuery string) (workflow.ListOptions, error) {
	opts := workflow.ListOptions{Limit: defaultLimit}
	if len(rawQuery) > maxQueryBytes {
		return opts, errors.New("query parameters are too long")
	}
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return opts, errors.New("query parameters are invalid")
	}
	for key, values := range query {
		switch key {
		case "workflowName", "status", "search", "limit", "offset":
		default:
			return opts, errors.New("an unknown query parameter was supplied")
		}
		if len(values) != 1 {
			return opts, errors.New("each query parameter must occur only once")
		}
	}

	opts.WorkflowName = query.Get("workflowName")
	opts.Search = query.Get("search")
	if !validText(opts.WorkflowName, maxFilterBytes) || !validText(opts.Search, maxFilterBytes) {
		return opts, errors.New(
			"filters must be valid text of at most 256 bytes without control characters",
		)
	}
	opts.Status = workflow.Status(query.Get("status"))
	if !validStatus(opts.Status) {
		return opts, errors.New("the workflow status filter is invalid")
	}
	if query.Has("limit") {
		limit, err := parseNonnegativeInt(query.Get("limit"))
		if err != nil || limit < 1 || limit > maxLimit {
			return opts, errors.New("limit must be an integer between 1 and 200")
		}
		opts.Limit = limit
	}
	if query.Has("offset") {
		offset, err := parseNonnegativeInt(query.Get("offset"))
		if err != nil {
			return opts, errors.New("offset must be a nonnegative integer")
		}
		opts.Offset = offset
	}
	return opts, nil
}

func parseNonnegativeInt(value string) (int, error) {
	if value == "" ||
		strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) != -1 {
		return 0, errors.New("invalid integer")
	}
	return strconv.Atoi(value)
}

func validText(value string, maxBytes int) bool {
	return len(value) <= maxBytes && utf8.ValidString(value) &&
		strings.IndexFunc(value, unicode.IsControl) == -1
}

func validStatus(status workflow.Status) bool {
	// Inspector statuses also include deferred states without changing the
	// aggregate's status constants. Validate their wire values at the HTTP seam.
	switch string(status) {
	case "", "running", "sleeping", "waiting", "retrying", "completed", "failed", "cancelled":
		return true
	default:
		return false
	}
}

func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().
		Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'; form-action 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("Cache-Control", "no-store")
}

type boundedBuffer struct {
	bytes.Buffer
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	if len(data) > maxResponseBytes-b.Len() {
		return 0, errors.New("monitor response exceeds size limit")
	}
	return b.Buffer.Write(data)
}

func writeJSON(w http.ResponseWriter, data any) {
	buffer := boundedBuffer{Buffer: bytes.Buffer{}}
	if err := json.NewEncoder(&buffer).Encode(data); err != nil {
		// Buffer before sending headers so oversized or invalid JSON never
		// produces a partial success response.
		writeError(
			w,
			http.StatusInternalServerError,
			"Unable to display workflow data within the response limit.",
		)
		return
	}
	writeBody(w, http.StatusOK, contentTypeJSON, buffer.Bytes())
}

type errorResponse struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, message string) {
	// This fixed struct contains only a string, so marshaling cannot fail.
	body, _ := json.Marshal(errorResponse{Error: message})
	writeBody(w, status, contentTypeJSON, body)
}

func writeBody(w http.ResponseWriter, status int, contentType string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	// A disconnected client cannot receive a replacement error response.
	_, _ = w.Write(body)
}
