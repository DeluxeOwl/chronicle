(() => {
  "use strict";

  // Resolve from this asset rather than the origin so StripPrefix mounts work.
  const baseURL = new URL("../", document.currentScript.src);
  const refreshInterval = 3000;
  const previewLimit = 32000;
  const $ = (id) => document.getElementById(id);
  const labels = {
    running: "Running",
    sleeping: "Sleeping",
    waiting: "Waiting for event",
    retrying: "Retry scheduled",
    completed: "Completed",
    failed: "Failed",
    cancelled: "Cancelled",
    timed_out: "Timed out",
  };
  const descriptions = {
    running: "Reported as running. Worker activity is not verified.",
    sleeping: "Suspended until the recorded wake time.",
    waiting: "Suspended until an external event arrives or its deadline expires.",
    retrying: "Waiting for the next retry attempt, not actively executing.",
    completed: "The workflow recorded a successful result.",
    failed: "The workflow recorded a failure.",
    cancelled: "The workflow recorded a cancellation.",
  };
  const state = {
    filters: { workflowName: "", status: "", search: "", limit: 50, offset: 0 },
    page: null,
    rows: new Map(),
    selectedId: null,
    selectedDetail: null,
    detailFingerprint: "",
    steps: new Map(),
    history: new Map(),
    busy: false,
    activeKind: null,
    pendingList: false,
    pendingDetail: false,
    controller: null,
    generation: 0,
    paused: false,
    timer: null,
    lastRefresh: null,
    refreshFailed: false,
  };

  function text(node, value) {
    const next = String(value ?? "");
    if (node.textContent !== next) node.textContent = next;
  }

  function element(tag, className, value) {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (value !== undefined) text(node, value);
    return node;
  }

  function knownTime(value) {
    if (!value || String(value).startsWith("0001-01-01")) return null;
    const date = new Date(value);
    return Number.isNaN(date.getTime()) ? null : date;
  }

  function formatTime(value) {
    const date = knownTime(value);
    if (!date) return "Not recorded";
    return date.toLocaleString(undefined, {
      month: "short", day: "numeric", year: "numeric",
      hour: "2-digit", minute: "2-digit", second: "2-digit", timeZoneName: "short",
    });
  }

  function timestamp(value) {
    const date = knownTime(value);
    if (!date) return element("span", "", "Not recorded");
    const node = element("time", "", formatTime(value));
    node.dateTime = date.toISOString();
    node.title = date.toISOString();
    return node;
  }

  function setTime(node, value) {
    const next = formatTime(value);
    if (node.textContent !== next) node.replaceChildren(timestamp(value));
  }

  function badge(node, status) {
    // Unknown values remain text, never CSS selectors or markup.
    node.dataset.status = Object.hasOwn(labels, status) ? status : "unknown";
    text(node, Object.hasOwn(labels, status) ? labels[status] : status || "Unknown");
  }

  function renderPreview(pre, note, value) {
    const formatted = JSON.stringify(value ?? null, null, 2);
    text(pre, formatted.length > previewLimit ? `${formatted.slice(0, previewLimit)}\n…` : formatted);
    note.hidden = formatted.length <= previewLimit;
  }

  function showError(node, message) {
    text(node, message);
    node.hidden = !message;
  }

  function updateActivity() {
    let message;
    if (state.paused) {
      message = "Auto-refresh paused";
    } else if (document.hidden) {
      message = "Auto-refresh paused while this tab is hidden";
    } else if (state.busy && !state.lastRefresh) {
      message = "Reading the event log…";
    } else if (state.refreshFailed) {
      message = "Refresh failed · showing the last available data";
    } else if (state.lastRefresh) {
      message = `Updated ${state.lastRefresh.toLocaleTimeString()} · auto-refresh every 3s`;
    } else {
      message = "Auto-refresh every 3s";
    }
    text($("refresh-status"), message);
    $("refresh").disabled = state.busy;
    $("pause").setAttribute("aria-pressed", String(state.paused));
    text($("pause"), state.paused ? "Resume auto-refresh" : "Pause auto-refresh");
    $("previous").disabled = state.busy || !state.page || state.filters.offset === 0;
    $("next").disabled = state.busy || !state.page || state.filters.offset + state.filters.limit >= state.page.total;
    $("list-region").setAttribute("aria-busy", String(state.busy && state.activeKind === "list"));
    $("detail").setAttribute("aria-busy", String(state.busy && state.activeKind === "detail"));
  }

  function scheduleRefresh() {
    clearTimeout(state.timer);
    if (!state.paused && !document.hidden && !state.busy) {
      state.timer = setTimeout(() => requestRefresh(), refreshInterval);
    }
  }

  function invalidateRequests(requeue = true) {
    state.generation += 1;
    if (requeue && state.activeKind === "list") state.pendingList = true;
    if (requeue && state.activeKind === "detail" && state.selectedId) state.pendingDetail = true;
    state.controller?.abort();
    clearTimeout(state.timer);
  }

  function requestRefresh(list = true, detail = true) {
    state.pendingList ||= list;
    state.pendingDetail ||= detail && Boolean(state.selectedId);
    clearTimeout(state.timer);
    void drainRequests();
  }

  async function fetchJSON(url, signal) {
    const response = await fetch(url, {
      method: "GET", signal, credentials: "same-origin", cache: "no-store",
      headers: { Accept: "application/json" },
    });
    if (!response.headers.get("Content-Type")?.includes("application/json")) {
      throw new Error("The API did not return JSON. Check your session and the monitor mount path.");
    }
    const data = await response.json();
    if (!response.ok) {
      const error = new Error(typeof data?.error === "string" ? data.error : `Request failed (${response.status}).`);
      error.status = response.status;
      throw error;
    }
    return data;
  }

  async function drainRequests() {
    if (state.busy || document.hidden) return;
    state.busy = true;
    while ((state.pendingList || state.pendingDetail) && !document.hidden) {
      // A newly selected instance gets priority. Requests are serialized, even
      // when a filter or selection changes during a slow refresh.
      const needsNewDetail = state.pendingDetail && state.selectedId && !state.selectedDetail;
      const kind = needsNewDetail ? "detail" : state.pendingList ? "list" : "detail";
      if (kind === "detail" && !state.selectedId) {
        state.pendingDetail = false;
        continue;
      }
      if (kind === "list") state.pendingList = false;
      else state.pendingDetail = false;
      const generation = state.generation;
      const selectedId = state.selectedId;
      const controller = new AbortController();
      state.controller = controller;
      state.activeKind = kind;
      updateActivity();
      try {
        const url = new URL(kind === "list" ? "api/workflows" : `api/workflows/${encodeURIComponent(selectedId)}`, baseURL);
        if (kind === "list") {
          for (const [key, value] of Object.entries(state.filters)) {
            if (value !== "") url.searchParams.set(key, String(value));
          }
        }
        const data = await fetchJSON(url, controller.signal);
        if (generation !== state.generation || (kind === "detail" && selectedId !== state.selectedId)) continue;
        const focused = document.activeElement;
        if (kind === "list") renderPage(data);
        else renderDetail(data);
        if (focused?.isConnected && document.activeElement !== focused && !focused.closest("[hidden]")) {
          focused.focus({ preventScroll: true });
        }
        state.lastRefresh = new Date();
        state.refreshFailed = false;
      } catch (error) {
        if (error.name === "AbortError" || generation !== state.generation) continue;
        state.refreshFailed = true;
        if (kind === "list") {
          showError($("list-error"), `${error.message} Use Refresh now to try again.`);
          if (!state.page) setListState("Workflows could not be loaded", "Check the connection, then refresh.");
        } else {
          const message = error.status === 404 ? "This workflow was not found in persisted history." : `${error.message} Use Refresh now to try again.`;
          showError($("detail-error"), message);
          $("detail-loading").hidden = true;
        }
      } finally {
        state.controller = null;
        state.activeKind = null;
      }
    }
    state.busy = false;
    updateActivity();
    scheduleRefresh();
  }

  function setListState(title, message) {
    text($("list-state-title"), title);
    text($("list-state-text"), message);
    $("list-state").hidden = false;
  }

  function newRow(id) {
    const row = element("tr");
    row.dataset.instanceId = id;
    const cell = element("td", "workflow-cell");
    const button = element("button", "workflow-button");
    button.type = "button";
    button.setAttribute("aria-controls", "detail");
    const name = element("span", "workflow-name");
    const instanceId = element("span", "workflow-id mono");
    button.append(name, instanceId);
    button.addEventListener("click", () => openDetail(id));
    cell.append(button);
    const statusCell = element("td");
    const status = element("span", "badge");
    const note = element("span", "status-note");
    statusCell.append(status, note);
    const attempt = element("td", "number");
    const timing = element("td", "timing");
    const started = element("span", "timing-line");
    const updated = element("span", "timing-line");
    timing.append(started, updated);
    row.append(cell, statusCell, attempt, timing);
    return { row, button, name, instanceId, status, note, attempt, started, updated };
  }

  function renderPage(page) {
    if (!Array.isArray(page?.instances) || !Number.isInteger(page.total) || page.total < 0) {
      throw new Error("The API returned an invalid workflow list.");
    }
    state.page = page;
    showError($("list-error"), "");
    if (state.filters.offset >= page.total && page.total > 0) {
      state.filters.offset = Math.floor((page.total - 1) / state.filters.limit) * state.filters.limit;
      state.pendingList = true;
      return;
    }
    const keep = new Set();
    let previous = null;
    for (const summary of page.instances) {
      const id = String(summary.instanceID);
      keep.add(id);
      let view = state.rows.get(id);
      if (!view) {
        view = newRow(id);
        state.rows.set(id, view);
      }
      text(view.name, summary.workflowName || "Unnamed workflow");
      text(view.instanceId, id);
      view.button.setAttribute("aria-label", `View ${summary.workflowName || "workflow"}, instance ${id}`);
      badge(view.status, summary.status);
      text(view.attempt, summary.attempt);
      const nextLabel = summary.status === "waiting" ? "Deadline" : summary.status === "sleeping" ? "Wake" : "Next run";
      const note = summary.waitingFor ? `Event: ${summary.waitingFor}` : summary.nextRunAt ? `${nextLabel}: ${formatTime(summary.nextRunAt)}` : `${summary.completedSteps ?? 0} steps completed`;
      text(view.note, note);
      setTimingLine(view.started, "Started", summary.startedAt);
      setTimingLine(view.updated, "Updated", summary.updatedAt);
      const nextNode = previous ? previous.nextSibling : $("workflow-rows").firstChild;
      if (nextNode !== view.row) $("workflow-rows").insertBefore(view.row, nextNode);
      previous = view.row;
    }
    for (const [id, view] of state.rows) {
      if (!keep.has(id)) {
        view.row.remove();
        state.rows.delete(id);
      }
    }
    updateSelection();
    text($("instance-count"), page.total);
    const from = page.instances.length ? state.filters.offset + 1 : 0;
    const to = state.filters.offset + page.instances.length;
    text($("page-summary"), `Showing ${from}–${page.instances.length ? to : 0} of ${page.total}`);
    $("list-state").hidden = page.instances.length > 0;
    if (!page.instances.length) {
      const filtered = state.filters.workflowName || state.filters.search || state.filters.status;
      setListState(filtered ? "No workflows match these filters" : "No workflows yet", filtered ? "Try another status or search term, or clear the filters." : "Instances appear here after a workflow starts and its events are persisted.");
    }
  }

  function setTimingLine(node, label, value) {
    const next = `${label} ${formatTime(value)}`;
    if (node.textContent !== next) node.replaceChildren(document.createTextNode(`${label} `), timestamp(value));
  }

  function updateSelection() {
    for (const [id, view] of state.rows) {
      const selected = id === state.selectedId;
      view.row.classList.toggle("selected", selected);
      view.button.setAttribute("aria-expanded", String(selected));
    }
  }

  function openDetail(id) {
    if (id === state.selectedId) {
      $("detail-title").focus();
      return;
    }
    invalidateRequests();
    state.selectedId = id;
    state.selectedDetail = null;
    state.detailFingerprint = "";
    state.steps.clear();
    state.history.clear();
    $("steps").replaceChildren();
    $("history").replaceChildren();
    $("detail").hidden = false;
    $("detail-content").hidden = true;
    $("detail-loading").hidden = false;
    $("workspace").classList.add("has-detail");
    text($("detail-title"), state.rows.get(id)?.name.textContent || "Workflow");
    text($("detail-id"), id);
    showError($("detail-error"), "");
    updateSelection();
    $("detail-title").focus();
    requestRefresh(false, true);
  }

  function closeDetail() {
    const id = state.selectedId;
    invalidateRequests();
    state.selectedId = null;
    state.selectedDetail = null;
    state.pendingDetail = false;
    $("detail").hidden = true;
    $("workspace").classList.remove("has-detail");
    updateSelection();
    (state.rows.get(id)?.button || $("apply")).focus();
    if (state.pendingList) void drainRequests();
    else scheduleRefresh();
  }

  function renderDetail(detail) {
    if (String(detail?.summary?.instanceID) !== state.selectedId || !Array.isArray(detail.steps) || !Array.isArray(detail.history)) {
      throw new Error("The API returned invalid workflow details.");
    }
    showError($("detail-error"), "");
    $("detail-loading").hidden = true;
    $("detail-content").hidden = false;
    state.selectedDetail = detail;
    const fingerprint = JSON.stringify(detail);
    if (fingerprint === state.detailFingerprint) return;
    state.detailFingerprint = fingerprint;
    const summary = detail.summary;
    text($("detail-title"), summary.workflowName || "Unnamed workflow");
    badge($("detail-status"), summary.status);
    text($("detail-status-description"), descriptions[summary.status] || "Persisted workflow state.");
    text($("detail-attempt"), summary.attempt);
    text($("detail-completed"), summary.completedSteps);
    text($("detail-version"), summary.version);
    setTime($("detail-started"), summary.startedAt);
    setTime($("detail-updated"), summary.updatedAt);
    $("finished-field").hidden = !knownTime(summary.finishedAt);
    setTime($("detail-finished"), summary.finishedAt);
    $("next-run-field").hidden = !knownTime(summary.nextRunAt);
    text($("next-run-label"), summary.status === "waiting" ? "Wait deadline" : summary.status === "sleeping" ? "Wake time" : "Next run");
    setTime($("detail-next-run"), summary.nextRunAt);
    $("waiting-field").hidden = !summary.waitingFor;
    text($("detail-waiting"), summary.waitingFor);
    $("workflow-error").hidden = !summary.error;
    text($("workflow-error-text"), summary.error);
    renderPreview($("params"), $("params-truncated"), detail.params);
    const hasOutput = Object.hasOwn(detail, "output");
    $("output-panel").hidden = !hasOutput;
    $("output-empty").hidden = hasOutput;
    if (hasOutput) renderPreview($("output"), $("output-truncated"), detail.output);
    renderSteps(detail.steps);
    renderHistory(detail.history);
  }

  function newPayload(label) {
    const panel = element("details", "payload");
    const summary = element("summary", "", label);
    const pre = element("pre");
    pre.tabIndex = 0;
    pre.setAttribute("aria-label", label);
    const note = element("p", "preview-note", "Preview limited to 32,000 characters. The API response contains the full value.");
    note.hidden = true;
    panel.append(summary, pre, note);
    return { panel, pre, note };
  }

  function newStep(index) {
    const item = element("li", "step-item");
    const marker = element("span", "step-marker", Number(index) + 1);
    marker.setAttribute("aria-hidden", "true");
    const heading = element("div", "step-heading");
    const headingText = element("div");
    const name = element("p", "step-name");
    const kind = element("span", "step-kind");
    const status = element("span", "badge");
    headingText.append(name, kind);
    heading.append(headingText, status);
    const times = element("div", "step-times");
    const error = element("p", "step-error");
    const result = newPayload("Step result");
    item.append(marker, heading, times, error, result.panel);
    return { item, name, kind, status, times, error, result, timesFingerprint: "" };
  }

  function renderSteps(steps) {
    const ordered = [...steps].sort((a, b) => a.index - b.index);
    const keep = new Set();
    for (const step of ordered) {
      keep.add(step.index);
      let view = state.steps.get(step.index);
      if (!view) {
        view = newStep(step.index);
        state.steps.set(step.index, view);
        $("steps").append(view.item);
      }
      text(view.name, step.name || `Step ${step.index + 1}`);
      text(view.kind, step.kind === "sleep" ? "Sleep checkpoint" : step.kind === "wait" ? "Event wait" : "Step");
      badge(view.status, step.status);
      view.item.dataset.status = view.status.dataset.status;
      const fingerprint = JSON.stringify([step.startedAt, step.completedAt, step.wakeAt, step.waitingFor]);
      if (fingerprint !== view.timesFingerprint) {
        view.timesFingerprint = fingerprint;
        view.times.replaceChildren();
        const times = [["Started", step.startedAt], ["Completed", step.completedAt], [step.kind === "wait" ? "Deadline" : "Wake", step.wakeAt]];
        let count = 0;
        for (const [label, value] of times) {
          if (!knownTime(value)) continue;
          const line = element("p");
          line.append(document.createTextNode(`${label}: `), timestamp(value));
          view.times.append(line);
          count += 1;
        }
        if (!count) view.times.append(element("p", "", "Timing not recorded"));
        if (step.waitingFor) view.times.append(element("p", "mono", `Event: ${step.waitingFor}`));
      }
      view.error.hidden = !step.error;
      text(view.error, step.error);
      const hasResult = Object.hasOwn(step, "result");
      view.result.panel.hidden = !hasResult;
      if (hasResult) renderPreview(view.result.pre, view.result.note, step.result);
    }
    // Reconcile order without replacing existing buttons or expanded results.
    let previous = null;
    for (const step of ordered) {
      const item = state.steps.get(step.index).item;
      const next = previous ? previous.nextSibling : $("steps").firstChild;
      if (item !== next) $("steps").insertBefore(item, next);
      previous = item;
    }
    for (const [index, view] of state.steps) {
      if (!keep.has(index)) {
        view.item.remove();
        state.steps.delete(index);
      }
    }
    text($("step-count"), steps.length);
    $("steps-empty").hidden = steps.length > 0;
  }

  function newHistoryEvent(key) {
    const panel = element("details", "history-event");
    const summary = element("summary");
    const version = element("span", "history-version");
    const name = element("span", "history-name");
    const time = element("span", "history-time");
    summary.append(version, name, time);
    const pre = element("pre");
    pre.tabIndex = 0;
    pre.setAttribute("aria-label", "Event data");
    const note = element("p", "preview-note", "Preview limited to 32,000 characters. The API response contains the full value.");
    note.hidden = true;
    panel.append(summary, pre, note);
    panel.addEventListener("toggle", () => {
      const current = state.history.get(key);
      if (panel.open && current) renderPreview(pre, note, current.data);
    });
    return { panel, version, name, time, pre, note, data: null };
  }

  function renderHistory(events) {
    const ordered = [...events].sort((a, b) => a.version - b.version);
    const keep = new Set();
    let previous = null;
    for (const event of ordered) {
      const key = `${event.version}:${event.eventName}`;
      keep.add(key);
      let view = state.history.get(key);
      if (!view) {
        view = newHistoryEvent(key);
        state.history.set(key, view);
      }
      text(view.version, `v${event.version}`);
      text(view.name, event.eventName);
      setTime(view.time, event.recordedAt);
      view.data = event.data;
      if (view.panel.open) renderPreview(view.pre, view.note, view.data);
      const next = previous ? previous.nextSibling : $("history").firstChild;
      if (view.panel !== next) $("history").insertBefore(view.panel, next);
      previous = view.panel;
    }
    for (const [key, view] of state.history) {
      if (!keep.has(key)) {
        view.panel.remove();
        state.history.delete(key);
      }
    }
    text($("history-count"), events.length);
    $("history-empty").hidden = events.length > 0;
  }

  $("filters").addEventListener("submit", (event) => {
    event.preventDefault();
    const workflowName = $("workflow-name").value;
    const search = $("search").value;
    if ([workflowName, search].some((value) => new TextEncoder().encode(value).length > 256)) {
      showError($("filter-error"), "Search and workflow name must each be at most 256 bytes.");
      return;
    }
    showError($("filter-error"), "");
    invalidateRequests();
    state.filters = { workflowName, search, status: $("status").value, limit: Number($("limit").value), offset: 0 };
    requestRefresh();
  });

  $("clear").addEventListener("click", () => {
    $("filters").reset();
    $("filters").requestSubmit();
  });
  $("previous").addEventListener("click", () => {
    invalidateRequests();
    state.filters.offset = Math.max(0, state.filters.offset - state.filters.limit);
    requestRefresh();
  });
  $("next").addEventListener("click", () => {
    invalidateRequests();
    state.filters.offset += state.filters.limit;
    requestRefresh();
  });
  $("refresh").addEventListener("click", () => requestRefresh());
  $("pause").addEventListener("click", () => {
    state.paused = !state.paused;
    if (state.paused) {
      // Finish any request already in progress. Only future polling stops;
      // explicit selections and Refresh now still work while paused.
      clearTimeout(state.timer);
    } else {
      requestRefresh();
    }
    updateActivity();
  });
  $("close-detail").addEventListener("click", closeDetail);
  $("detail").addEventListener("keydown", (event) => {
    if (event.key === "Escape") {
      event.preventDefault();
      closeDetail();
    }
  });
  document.addEventListener("visibilitychange", () => {
    if (document.hidden) {
      invalidateRequests(false);
      state.pendingList = false;
      state.pendingDetail = false;
    } else if (!state.paused) {
      requestRefresh();
    }
    updateActivity();
  });
  window.addEventListener("pagehide", () => {
    invalidateRequests(false);
    state.pendingList = false;
    state.pendingDetail = false;
  });
  window.addEventListener("pageshow", (event) => {
    if (event.persisted && !state.paused) requestRefresh();
  });

  requestRefresh();
})();
