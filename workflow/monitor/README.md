# Embedded workflow monitor

`workflow/monitor` serves a read-only HTML UI and JSON API. It uses Go's standard library and embedded assets. It needs no frontend build, external fonts, or CDN.

## Embed in a server

Create an inspector from the same finite `event.GlobalLog` that stores your workflows:

```go
import (
    "net/http"

    "github.com/DeluxeOwl/chronicle/event"
    "github.com/DeluxeOwl/chronicle/workflow"
    "github.com/DeluxeOwl/chronicle/workflow/monitor"
)

func mountMonitor(mux *http.ServeMux, log event.GlobalLog) error {
    inspector, err := workflow.NewInspector(log)
    if err != nil {
        return err
    }
    handler, err := monitor.NewHandler(inspector)
    if err != nil {
        return err
    }
    // Protect this handler with your authentication and authorization middleware.
    mux.Handle("/workflows/", http.StripPrefix("/workflows", handler))
    return nil
}
```

Open `/workflows/`, with the trailing slash. Assets and API URLs work under nested mount prefixes too.

**Security:** The host must authenticate and authorize the whole handler, including its API and assets. Parameters, results, errors, and history can contain secrets or personal data. Read-only access does not make this data safe to publish. The monitor does not provide authentication or cross-origin access.

## UI

- Search by workflow name or instance ID. Filter by exact workflow name and status.
- View instance IDs, status, attempts, and recorded timestamps in a table. Change the page size or use Previous and Next.
- Select a workflow to view its recorded step path, parameters, output, errors, and chronological event history.
- Expand individual history events and step results. Refresh preserves expanded panels and keyboard focus on existing controls.
- Automatic refresh runs three seconds after requests finish. Requests do not overlap. Hidden tabs stop polling. Pause stops automatic refresh. Refresh now still works while paused.

Sleep, event waits, and scheduled retries have separate labels. They do not indicate active execution. Running means the persisted state reports running, not that a worker is alive. The monitor does not infer a static DAG or invent durations. Missing legacy timestamps show **Not recorded**.

Payload previews stop at 32,000 characters. The JSON API contains the full payload within the response limit. History is never silently truncated.

## HTTP API

| Request | Result |
| --- | --- |
| `GET /` | Embedded HTML page |
| `GET /assets/app.js` | UI JavaScript |
| `GET /assets/style.css` | UI stylesheet |
| `GET /assets/favicon.svg` | Embedded icon |
| `GET /api/workflows` | `{ "instances": [...], "total": 0 }` |
| `GET /api/workflows/{id}` | Summary, parameters, output, steps, and history |

List query parameters:

| Parameter | Rule |
| --- | --- |
| `workflowName` | Exact match, at most 256 UTF-8 bytes |
| `search` | Case-insensitive name or ID match, at most 256 UTF-8 bytes |
| `status` | `running`, `sleeping`, `waiting`, `retrying`, `completed`, `failed`, or `cancelled` |
| `limit` | Integer from 1 to 200. Default: 50 |
| `offset` | Nonnegative integer. Default: 0 |

Empty text filters mean no filter. Unknown or duplicate parameters, control characters, malformed values, and queries longer than 4,096 bytes return `400`. Detail IDs have a 512-byte limit. Detail requests do not accept query parameters.

Known routes accept only `GET`. Other methods return `405` with `Allow: GET`. Missing workflows and unknown routes return `404`. Read errors return `500` without internal error details. API errors use `{ "error": "..." }`.

Queries have a ten-second timeout. JSON responses have a 4 MiB limit. Larger responses return a complete `500` error, not partial JSON. All responses use `Cache-Control: no-store` and `X-Content-Type-Options: nosniff`. A restrictive CSP permits only same-origin assets and API connections.

## Limits

The inspector reads durable history without a workflow registry or separate SQL tables. It scans the event log on each list query. Use an indexed projection for large installations. Discovery requires a finite, non-tailing global log. NATS does not currently support workflow discovery.

The monitor has no cancel, retry, or other mutation routes. Host middleware, server timeouts, and rate limits remain the host's responsibility.
