// Package monitor serves an embedded, read-only workflow UI and JSON API.
// It uses persisted workflow history and does not execute workflow code.
//
// Mount the handler at a URL with a trailing slash:
//
//	inspector, err := workflow.NewInspector(globalLog)
//	// Handle err.
//	handler, err := monitor.NewHandler(inspector)
//	// Handle err.
//	mux.Handle("/workflows/", http.StripPrefix("/workflows", handler))
//
// The host must authenticate and authorize every route, including the JSON API
// and assets. Parameters, results, errors, and event history can expose secrets
// or personal data. This package does not provide authentication or CORS access.
//
// GET / serves the page. GET /api/workflows accepts workflowName, status, search,
// limit, and offset. Limit defaults to 50 and must be between 1 and 200 when set.
// Offset must be nonnegative. Filters have a 256-byte limit. Duplicate or unknown
// parameters return 400. GET /api/workflows/{id} returns instance details.
// All other methods return 405 for known routes. Missing workflows return 404.
// Queries have a ten-second timeout. JSON responses have a 4 MiB limit. Oversized
// responses return 500 without partial JSON. History is not silently truncated.
//
// The UI refreshes every three seconds after requests finish. Refresh pauses
// while the tab is hidden. Users can pause automatic refresh or refresh manually.
// The inspector scans durable events on each query, so this monitor suits small
// installations. Large installations need a separate indexed projection.
// Discovery requires event.GlobalLog. NATS does not currently support discovery.
package monitor
