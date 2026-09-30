(() => {
  "use strict";

  const state = {
    token: sessionStorage.getItem("wardstone.operatorToken") || "",
    actor: sessionStorage.getItem("wardstone.operatorActor") || "",
    investigations: [],
    approvals: [],
  };
  const byId = (id) => document.getElementById(id);
  const connection = byId("connection");
  const dashboard = byId("dashboard");
  const tokenInput = byId("operator-token");
  const connectionError = byId("connection-error");
  const refreshButton = byId("refresh");
  const sessionButton = byId("session-button");
  const toast = byId("toast");
  let toastTimer;

  function node(tag, options = {}, children = []) {
    const element = document.createElement(tag);
    for (const [key, value] of Object.entries(options)) {
      if (key === "className") element.className = value;
      else if (key === "text") element.textContent = value;
      else if (key.startsWith("data-")) element.setAttribute(key, value);
      else element[key] = value;
    }
    for (const child of children) element.append(child);
    return element;
  }

  async function api(path, options = {}) {
    const headers = new Headers(options.headers || {});
    headers.set("Authorization", `Bearer ${state.token}`);
    if (options.body) headers.set("Content-Type", "application/json");
    const response = await fetch(path, { ...options, headers, cache: "no-store" });
    let payload = {};
    try { payload = await response.json(); } catch (_) { /* empty response */ }
    if (!response.ok) {
      const error = new Error(payload.error || `Request failed (${response.status})`);
      error.status = response.status;
      throw error;
    }
    return payload;
  }

  async function checkHealth() {
    try {
      const response = await fetch("/healthz", { cache: "no-store" });
      if (!response.ok) throw new Error("unhealthy");
      byId("health-dot").className = "health-dot ok";
      byId("health-label").textContent = "Service online";
    } catch (_) {
      byId("health-dot").className = "health-dot bad";
      byId("health-label").textContent = "Service unavailable";
    }
  }

  async function loadDashboard({ quiet = false } = {}) {
    if (!state.token) return;
    refreshButton.disabled = true;
    if (!quiet) connectionError.textContent = "";
    try {
      const [overview, investigationPayload, approvalPayload] = await Promise.all([
        api("/v1/admin/overview"),
        api("/v1/admin/investigations?limit=100"),
        api("/v1/admin/approvals?status=PENDING"),
      ]);
      state.investigations = investigationPayload.investigations || [];
      state.approvals = approvalPayload.approvals || [];
      renderOverview(overview);
      renderApprovals();
      renderInvestigations();
      connection.hidden = true;
      dashboard.hidden = false;
      sessionButton.textContent = "Disconnect";
      refreshButton.disabled = false;
      byId("last-updated").textContent = `Updated ${new Date().toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}`;
    } catch (error) {
      refreshButton.disabled = false;
      if (error.status === 401) {
        disconnect();
        connectionError.textContent = "The operator token was not accepted.";
      } else if (!quiet) {
        connectionError.textContent = error.message;
        showToast(error.message);
      }
    }
  }

  function renderOverview(overview) {
    const investigations = overview.investigations || {};
    const approvals = overview.approvals || {};
    const jobs = overview.jobs || {};
    byId("metric-active").textContent = (investigations.pending || 0) + (investigations.running || 0);
    byId("metric-approvals").textContent = approvals.pending || 0;
    byId("metric-completed").textContent = investigations.completed || 0;
    byId("metric-attention").textContent = (investigations.failed || 0) + (jobs.dead || 0);
    byId("metric-completed-note").textContent = `${investigations.total || 0} investigations total`;
    byId("mode-badge").textContent = `${overview.mode || "UNKNOWN"} MODE`;
    const badge = byId("approval-badge");
    badge.textContent = approvals.pending || 0;
    badge.hidden = !approvals.pending;
  }

  function renderApprovals() {
    const list = byId("approval-list");
    list.replaceChildren();
    if (!state.approvals.length) {
      list.append(emptyState("✓", "Approval queue is clear", "There are no pending operator decisions."));
      return;
    }
    for (const approval of state.approvals) {
      const grant = node("button", { className: "button", type: "button", text: "Grant", "data-id": approval.id, "data-decision": "GRANTED" });
      const deny = node("button", { className: "button ghost", type: "button", text: "Deny", "data-id": approval.id, "data-decision": "DENIED" });
      const expires = new Date(approval.expires_at);
      const meta = node("p", { className: "approval-meta" }, [
        node("span", { text: approval.ticket?.external_id || approval.investigation_id }),
        node("span", { text: approval.capability }),
        node("span", { text: `Expires ${formatRelative(expires)}` }),
      ]);
      const content = node("div", {}, [
        node("h3", { text: approval.ticket?.summary || "Proposed action" }),
        meta,
        node("p", { className: "approval-reason", text: approval.reason || approval.policy_reason || "No reason provided." }),
      ]);
      list.append(node("article", { className: "approval-card" }, [content, node("div", { className: "approval-actions" }, [deny, grant])]));
    }
  }

  function renderInvestigations() {
    const tbody = byId("investigation-list");
    tbody.replaceChildren();
    byId("investigation-empty").hidden = state.investigations.length > 0;
    for (const investigation of state.investigations) {
      const ticket = node("td", { className: "ticket-cell" }, [
        node("strong", { text: investigation.ticket?.external_id || investigation.id }),
        node("small", { text: investigation.ticket?.summary || "No summary" }),
      ]);
      const status = node("span", { className: `status ${String(investigation.status).toLowerCase()}`, text: investigation.status });
      const view = node("button", { className: "row-button", type: "button", text: "View →", "data-investigation-id": investigation.id });
      tbody.append(node("tr", {}, [
        ticket,
        node("td", {}, [status]),
        node("td", { className: "diagnosis-cell", text: investigation.failure || investigation.diagnosis || "Investigation in progress" }),
        node("td", { text: formatDate(investigation.created_at) }),
        node("td", {}, [view]),
      ]));
    }
  }

  function emptyState(icon, title, copy) {
    return node("div", { className: "empty-state" }, [node("span", { text: icon }), node("h3", { text: title }), node("p", { text: copy })]);
  }

  async function showInvestigation(id) {
    const sessionToken = state.token;
    const dialog = byId("investigation-dialog");
    const body = byId("dialog-body");
    byId("dialog-title").textContent = id;
    body.replaceChildren(node("p", { text: "Loading investigation…" }));
    dialog.showModal();
    try {
      const [investigation, timelinePayload] = await Promise.all([
        api(`/v1/investigations/${encodeURIComponent(id)}`),
        api(`/v1/investigations/${encodeURIComponent(id)}/timeline`),
      ]);
      if (state.token !== sessionToken) return;
      const fields = node("div", { className: "detail-grid" }, [
        detailField("Status", investigation.status), detailField("Ticket ID", investigation.ticket_id),
        detailField("Current specialist", investigation.specialist || "Unassigned"), detailField("Model", investigation.model || "—"), detailField("Prompt", investigation.prompt_version || "—"),
        detailField("Started", formatDate(investigation.started_at)), detailField("Completed", formatDate(investigation.completed_at)),
      ]);
      const timeline = node("ol", { className: "timeline" });
      for (const event of timelinePayload.events || []) {
        const entry = node("li", {}, [node("strong", { text: humanize(event.type) }), node("small", { text: `${formatDate(event.occurred_at)} · ${event.actor_id || event.actor_type}` })]);
        const description = describeRoutingEvent(event);
        if (description) entry.append(node("p", { text: description }));
        timeline.append(entry);
      }
      body.replaceChildren(fields, routingCard(investigation.routing), node("h3", { text: investigation.failure ? "Failure" : "Diagnosis" }), node("p", { className: "diagnosis", text: investigation.failure || investigation.diagnosis || "No diagnosis yet." }), node("h3", { text: "Audit timeline" }), timeline);
    } catch (error) {
      if (state.token !== sessionToken) return;
      body.replaceChildren(emptyState("!", "Could not load investigation", error.message));
    }
  }

  function detailField(label, value) {
    return node("div", { className: "detail-field" }, [node("span", { text: label }), node("strong", { text: value || "—" })]);
  }

  function routingCard(routing, { preview = false } = {}) {
    const card = node("article", { className: "routing-card" }, [node("h3", { text: preview ? "Routing result" : "Initial ticket routing" })]);
    if (!routing) {
      card.append(node("p", { className: "routing-reason", text: "Routing details were not recorded for this investigation." }));
      return card;
    }
    const fields = [
      detailField("Classification", routing.classification),
      detailField("Initial specialist", routing.specialist),
      detailField("Source", routing.source),
    ];
    if (routing.rule_name) fields.push(detailField("Matched rule", routing.rule_name));
    if (routing.confidence != null) fields.push(detailField("Confidence", `${Math.round(routing.confidence * 100)}%`));
    if (routing.fallback_code) fields.push(detailField("Fallback", routing.fallback_code));
    if (routing.model_provider || routing.model) fields.push(detailField("Classification model", [routing.model_provider, routing.model].filter(Boolean).join(" / ")));
    card.append(node("div", { className: "detail-grid" }, fields), node("p", { className: "routing-reason", text: routing.reason || "No routing reason recorded." }));
    if (preview) card.append(node("p", { className: "routing-copy", text: routing.source === "rule" ? "The ticket matched a routing rule; model intent classification is unnecessary." : "No rule matched. The preview stops before model intent classification; the reason above explains the configured fallback." }));
    return card;
  }

  function describeRoutingEvent(event) {
    const data = event.data || {};
    if (event.type === "dispatcher.routed") {
      const routing = data.routing || data;
      return [routing.specialist && `Initial specialist: ${routing.specialist}`, routing.classification && `Classification: ${routing.classification}`, routing.source && `Source: ${routing.source}`, routing.rule_name && `Rule: ${routing.rule_name}`, routing.fallback_code && `Fallback: ${routing.fallback_code}`, routing.reason].filter(Boolean).join(" · ");
    }
    if (event.type === "specialist.handed_off") {
      return [data.from && data.to ? `${data.from} → ${data.to}` : "Specialist handoff", data.reason].filter(Boolean).join(" · ");
    }
    return "";
  }

  async function submitRoutingPreview(event) {
    event.preventDefault();
    const sessionToken = state.token;
    const submit = byId("routing-preview-submit");
    const result = byId("routing-preview-result");
    const errorBox = byId("routing-preview-error");
    submit.disabled = true;
    errorBox.textContent = "";
    result.replaceChildren();
    try {
      const payload = JSON.parse(byId("routing-payload").value);
      if (!payload || typeof payload !== "object" || Array.isArray(payload)) throw new Error("Enter one Jira ticket JSON object.");
      const response = await api("/v1/admin/routing/preview", { method: "POST", body: JSON.stringify(payload) });
      if (state.token !== sessionToken) return;
      result.replaceChildren(routingCard(response.routing, { preview: true }));
    } catch (error) {
      if (state.token !== sessionToken) return;
      errorBox.textContent = error instanceof SyntaxError ? "Enter valid JSON for the Jira ticket payload." : error.message;
    } finally {
      submit.disabled = false;
    }
  }

  function openDecision(id, decision) {
    const approval = state.approvals.find((item) => item.id === id);
    byId("decision-id").value = id;
    byId("decision-value").value = decision;
    byId("decision-actor").value = state.actor;
    byId("decision-error").textContent = "";
    byId("decision-title").textContent = decision === "GRANTED" ? "Grant approval" : "Deny approval";
    byId("decision-copy").textContent = `${decision === "GRANTED" ? "Grant" : "Deny"} ${approval?.capability || "this action"} for ${approval?.ticket?.external_id || "this request"}? The decision is final and will be audit logged.`;
    const submit = byId("decision-submit");
    submit.textContent = decision === "GRANTED" ? "Grant approval" : "Deny approval";
    submit.className = decision === "GRANTED" ? "button" : "button danger";
    byId("decision-dialog").showModal();
  }

  async function submitDecision(event) {
    event.preventDefault();
    const id = byId("decision-id").value;
    const decision = byId("decision-value").value;
    const actor = byId("decision-actor").value.trim();
    const submit = byId("decision-submit");
    submit.disabled = true;
    byId("decision-error").textContent = "";
    try {
      await api(`/v1/admin/approvals/${encodeURIComponent(id)}/decision`, { method: "POST", body: JSON.stringify({ decision, actor }) });
      state.actor = actor;
      sessionStorage.setItem("wardstone.operatorActor", actor);
      byId("decision-dialog").close();
      showToast(`Approval ${decision.toLowerCase()}.`);
      await loadDashboard({ quiet: true });
    } catch (error) {
      byId("decision-error").textContent = error.message;
    } finally {
      submit.disabled = false;
    }
  }

  function disconnect() {
    state.token = "";
    state.investigations = [];
    state.approvals = [];
    sessionStorage.removeItem("wardstone.operatorToken");
    tokenInput.value = "";
    connection.hidden = false;
    dashboard.hidden = true;
    sessionButton.textContent = "Connect";
    refreshButton.disabled = true;
    byId("routing-preview-result").replaceChildren();
    byId("routing-preview-error").textContent = "";
    byId("investigation-dialog").close();
    byId("decision-dialog").close();
  }

  function showToast(message) {
    clearTimeout(toastTimer);
    toast.textContent = message;
    toast.hidden = false;
    toastTimer = setTimeout(() => { toast.hidden = true; }, 3500);
  }

  function formatDate(value) {
    if (!value) return "—";
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return "—";
    return date.toLocaleString([], { dateStyle: "medium", timeStyle: "short" });
  }

  function formatRelative(date) {
    if (Number.isNaN(date.getTime())) return "at an unknown time";
    const minutes = Math.round((date.getTime() - Date.now()) / 60000);
    if (minutes <= 0) return "overdue";
    if (minutes < 60) return `in ${minutes}m`;
    if (minutes < 1440) return `in ${Math.round(minutes / 60)}h`;
    return `in ${Math.round(minutes / 1440)}d`;
  }

  function humanize(value) {
    return String(value || "event").replaceAll(".", " · ").replaceAll("_", " ").replace(/\b\w/g, (letter) => letter.toUpperCase());
  }

  byId("connection-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    state.token = tokenInput.value;
    sessionStorage.setItem("wardstone.operatorToken", state.token);
    await loadDashboard();
  });
  sessionButton.addEventListener("click", () => state.token ? disconnect() : tokenInput.focus());
  refreshButton.addEventListener("click", () => loadDashboard());
  byId("approval-list").addEventListener("click", (event) => {
    const button = event.target.closest("button[data-decision]");
    if (button) openDecision(button.dataset.id, button.dataset.decision);
  });
  byId("investigation-list").addEventListener("click", (event) => {
    const button = event.target.closest("button[data-investigation-id]");
    if (button) showInvestigation(button.dataset.investigationId);
  });
  byId("dialog-close").addEventListener("click", () => byId("investigation-dialog").close());
  document.querySelectorAll(".dialog-cancel").forEach((button) => button.addEventListener("click", () => byId("decision-dialog").close()));
  byId("decision-form").addEventListener("submit", submitDecision);
  byId("routing-preview-form").addEventListener("submit", submitRoutingPreview);
  document.querySelectorAll(".nav-item").forEach((link) => link.addEventListener("click", () => {
    document.querySelectorAll(".nav-item").forEach((item) => item.classList.remove("active"));
    link.classList.add("active");
  }));
  document.addEventListener("visibilitychange", () => { if (!document.hidden && state.token) loadDashboard({ quiet: true }); });

  checkHealth();
  setInterval(checkHealth, 30000);
  setInterval(() => { if (!document.hidden && state.token) loadDashboard({ quiet: true }); }, 30000);
  if (state.token) loadDashboard();
})();
