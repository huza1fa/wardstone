(() => {
  "use strict";

  const state = {
    token: sessionStorage.getItem("wardstone.operatorToken") || "",
    actor: sessionStorage.getItem("wardstone.operatorActor") || "",
    investigations: [],
    approvals: [],
    setup: null,
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
  let setupRevision = 0;
  let activeSetupProbe = "";
  let detailRevision = 0;
  let dashboardRevision = 0;
  let createCaseRevision = 0;
  let proposalRevision = 0;

  function node(tag, options = {}, children = []) {
    const element = document.createElement(tag);
    for (const [key, value] of Object.entries(options)) {
      if (key === "className") element.className = value;
      else if (key === "text") element.textContent = value;
      else if (key.startsWith("data-") || key.startsWith("aria-")) element.setAttribute(key, value);
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
    const sessionToken=state.token, revision=++dashboardRevision;
    refreshButton.disabled = true;
    if (!quiet) connectionError.textContent = "";
    try {
      const [overview, investigationPayload, approvalPayload] = await Promise.all([
        api("/v1/admin/overview"),
        api("/v1/admin/investigations?limit=100"),
        api("/v1/admin/approvals?status=PENDING"),
      ]);
      if (state.token!==sessionToken||revision!==dashboardRevision) return;
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
      loadSetup({ quiet });
      loadTestExecutions();
    } catch (error) {
      if (state.token!==sessionToken||revision!==dashboardRevision) return;
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

  async function loadSetup({ quiet = false } = {}) {
    if (activeSetupProbe) return;
    const revision = ++setupRevision;
    const sessionToken = state.token;
    try {
      const setup = await api("/v1/admin/setup");
      if (state.token !== sessionToken || revision !== setupRevision) return;
      state.setup = setup;
      byId("setup-error").textContent = "";
      renderSetup();
    } catch (error) {
      if (state.token !== sessionToken || revision !== setupRevision) return;
      if (error.status === 401) {
        disconnect();
        connectionError.textContent = "The operator token was not accepted.";
      } else {
        byId("setup-error").textContent = error.message;
        if (!quiet) showToast(error.message);
      }
    }
  }

  function renderSetup() {
    const setup = state.setup || {};
    const overall = String(setup.state || "unknown");
    const stateBadge = byId("setup-state");
    stateBadge.className = `setup-state ${overall}`;
    stateBadge.textContent = humanize(overall);
    const features = byId("setup-features");
    features.replaceChildren();
    for (const [name, enabled] of Object.entries(setup.features || {})) {
      features.append(node("span", { className: `feature ${enabled ? "enabled" : "disabled"}`, text: `${humanize(name)}: ${enabled ? "enabled" : "disabled"}` }));
    }
    const list = byId("setup-list");
    list.replaceChildren();
    for (const component of setup.components || []) {
      const componentState = String(component.state || "unknown");
      const header = node("div", { className: "setup-card-head" }, [
        node("div", {}, [node("p", { className: "setup-requirement", text: component.required ? "Required" : "Optional" }), node("h3", { text: component.label || component.name })]),
        node("span", { className: `status ${componentState}`, text: humanize(componentState) }),
      ]);
      const children = [
        header,
        node("p", { className: "setup-description", text: component.description || "" }),
        node("p", { className: "setup-permission", text: component.permission || "" }),
        node("p", { className: "setup-message", text: component.message || "" }),
      ];
      if (component.checked_at) children.push(node("small", { className: "setup-checked", text: `Tested ${formatDate(component.checked_at)}${component.duration_ms ? ` · ${component.duration_ms} ms` : ""}` }));
      if (component.configured && component.probeable) children.push(node("button", { className: "button ghost setup-test", type: "button", disabled: Boolean(activeSetupProbe), text: activeSetupProbe === component.name ? "Testing…" : "Test connection", "data-setup-name": component.name }));
      list.append(node("article", { className: "setup-card" }, children));
    }
  }

  async function testSetupConnection(button) {
    if (activeSetupProbe) return;
    const name = button.dataset.setupName;
    const sessionToken = state.token;
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), 6500);
    const revision = ++setupRevision;
    activeSetupProbe = name;
    button.disabled = true;
    button.textContent = "Testing…";
    byId("setup-error").textContent = "";
    renderSetup();
    try {
      const response = await api(`/v1/admin/setup/connectors/${encodeURIComponent(name)}/test`, { method: "POST", signal: controller.signal });
      if (state.token !== sessionToken || revision !== setupRevision) return;
      state.setup = response.setup || state.setup;
    } catch (error) {
      if (state.token !== sessionToken || revision !== setupRevision) return;
      if (error.status === 401) disconnect();
      else byId("setup-error").textContent = error.name === "AbortError" ? "The connection test timed out." : error.message;
    } finally {
      clearTimeout(timeout);
      if (revision === setupRevision) activeSetupProbe = "";
      button.disabled = false;
      button.textContent = "Test connection";
      if (state.token === sessionToken && revision === setupRevision) renderSetup();
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
        node("pre", { className: "literal-json", text: JSON.stringify(approval.arguments || {}, null, 2) }),
        node("code", { className: "detail-copy", text: `Action SHA-256: ${approval.action_digest || "Not recorded"}` }),
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
    const revision = ++detailRevision;
    const sessionToken = state.token;
    const dialog = byId("investigation-dialog");
    const body = byId("dialog-body");
    byId("dialog-title").textContent = id;
    body.replaceChildren(node("p", { text: "Loading investigation…" }));
    dialog.showModal();
    try {
      const [investigation, timelinePayload, messagesPayload, casePayload] = await Promise.all([
        api(`/v1/investigations/${encodeURIComponent(id)}`),
        api(`/v1/investigations/${encodeURIComponent(id)}/timeline`),
        api(`/v1/investigations/${encodeURIComponent(id)}/messages`).catch((error) => {
          if (error.status === 401) throw error;
          return { error: "Conversation could not be loaded." };
        }),
        api(`/v1/admin/investigations/${encodeURIComponent(id)}/case`).catch((error) => { if (error.status === 401) throw error; return {}; }),
      ]);
      if (state.token !== sessionToken || revision !== detailRevision) return;
      const fields = node("div", { className: "detail-grid" }, [
        detailField("Status", investigation.status), detailField("Ticket ID", investigation.ticket_id),
        detailField("Current specialist", investigation.specialist || "Unassigned"), detailField("Model", investigation.model || "—"), detailField("Prompt", investigation.prompt_version || "—"),
        detailField("Started", formatDate(investigation.started_at)), detailField("Completed", formatDate(investigation.completed_at)),
      ]);
      const timeline = node("ol", { className: "timeline" });
      const events = timelinePayload.events || [];
      const evidence = evidenceIndex(events);
      for (const event of events) {
        const entry = node("li", {}, [node("strong", { text: humanize(event.type) }), node("small", { text: `${formatDate(event.occurred_at)} · ${event.actor_id || event.actor_type}` })]);
        const description = describeRoutingEvent(event);
        if (description) entry.append(node("p", { text: description }));
        if (event.type === "evidence.collected") entry.append(evidenceCitation(event.data?.evidence_id, evidence));
        if (["vendor.risk_scored", "requester.send_started", "requester.send_retried", "requester.message_failed", "requester.message_delivered", "requester.delivery_checked", "requester.delivery_reconciled", "requester.resend_requested"].includes(event.type)) entry.append(node("pre", { className: "literal-json", text: JSON.stringify(event.data || {}, null, 2) }));
        if (event.type === "action.proposed") entry.append(node("pre", { className: "literal-json", text: JSON.stringify(event.data?.arguments ?? null, null, 2) }));
        timeline.append(entry);
      }
      body.replaceChildren(caseControls(investigation, casePayload, id), fields, routingCard(investigation.routing), reportCard(investigation.work_package, evidence), node("h3", { text: investigation.failure ? "Failure" : "Diagnosis" }), node("p", { className: "diagnosis", text: investigation.failure || investigation.diagnosis || "No diagnosis yet." }), actionsCard(events, evidence), conversationCard(messagesPayload, id, investigation.status, casePayload.ticket?.source), node("h3", { text: "Audit timeline" }), timeline);
    } catch (error) {
      if (state.token !== sessionToken || revision !== detailRevision) return;
      body.replaceChildren(emptyState("!", "Could not load investigation", error.message));
    }
  }

  let testExecutionRevision = 0;
  async function loadTestExecutions() {
    const revision = ++testExecutionRevision, token = state.token;
    try {
      const payload = await api("/v1/admin/test-executions");
      if (revision !== testExecutionRevision || token !== state.token) return;
      const items = payload.items || [];
      byId("test-execution").hidden = !payload.enabled && !items.length;
      byId("test-proposal-form").hidden = !payload.enabled;
      byId("test-execution-error").textContent = payload.enabled ? "" : "Test execution is paused. History remains available.";
      const list = byId("test-execution-list"); list.replaceChildren();
      for (const item of items) {
        const action = item.action || {}, approval = item.approval || {}, execution = item.execution;
        const card = node("article", { className: "report-card" }, [node("h3", { text: "Synthetic group addition" }), detailField("Approval", approval.status), detailField("Execution", execution?.status || "Not queued"), node("pre", { className: "literal-json", text: JSON.stringify(action.arguments || {}, null, 2) }), detailField("Action SHA-256", action.digest), node("p", { className: "detail-copy", text: action.reason || "" })]);
        const view = node("button", { type: "button", className: "button ghost", text: "View case and audit" });
        view.addEventListener("click", () => showInvestigation(action.investigation_id)); card.append(view);
        if (execution?.error) card.append(detailField("Recovery detail", execution.error));
        if (item.verification) card.append(detailField("Last independent check", item.verification.succeeded ? "Membership and operation receipt verified" : "Postcondition not verified"), node("pre", { className: "literal-json", text: JSON.stringify(item.verification.details || {}, null, 2) }));
        let path = "", label = "";
        if (payload.enabled && approval.status === "GRANTED" && !execution) { path = `/v1/admin/test-executions/approvals/${encodeURIComponent(approval.id)}/queue`; label = "Queue approved test execution"; }
        if (payload.enabled && ["FAILED", "NEEDS_RECONCILIATION"].includes(execution?.status)) { path = `/v1/admin/test-executions/${encodeURIComponent(execution.id)}/verify`; label = "Verify again — read only"; }
        if (path) {
          const form = node("form", { className: "action-detail case-command" });
          const actor = node("input", { required: true, value: state.actor, maxLength: 256, placeholder: "Operator name", "aria-label": "Operator name" });
          const reason = node("input", { required: true, maxLength: 1000, placeholder: "Reason", "aria-label": "Reason" });
          const error = node("p", { className: "form-error", role: "alert" });
          const submit = node("button", { type: "submit", className: "button", text: label });
          form.append(actor, reason, error, submit);
          form.addEventListener("submit", async (event) => {
            event.preventDefault(); if (submit.disabled) return;
            const command = { actor: actor.value.trim(), reason: reason.value.trim() };
            if (!command.actor || !command.reason) { error.textContent = "Operator name and reason are required."; return; }
            command.request_id = commandKey(form, command); submit.disabled = true; error.textContent = "";
            const commandToken = state.token, commandRevision = testExecutionRevision;
            try {
              await api(path, { method: "POST", body: JSON.stringify(command) });
              if (state.token !== commandToken || testExecutionRevision !== commandRevision) return;
              state.actor = command.actor; sessionStorage.setItem("wardstone.operatorActor", command.actor); await loadTestExecutions();
            } catch (failure) { if (state.token === commandToken && testExecutionRevision === commandRevision) error.textContent = failure.message; }
            finally { submit.disabled = false; }
          });
          card.append(form);
        }
        list.append(card);
      }
    } catch (error) { if (revision === testExecutionRevision && token === state.token) byId("test-execution-error").textContent = error.message; }
  }

  async function proposeTestExecution(event) {
    event.preventDefault(); const form = byId("test-proposal-form"), button = byId("test-proposal-submit"), error = byId("test-proposal-error");
    if (button.disabled) return;
    const payload = { actor: byId("test-actor").value.trim(), reason: byId("test-reason").value.trim(), group: byId("test-group").value.trim(), member: byId("test-member").value.trim() };
    if (!payload.actor || !payload.reason || !/^test-[a-z0-9][a-z0-9_-]{0,58}$/.test(payload.group) || !/^test-[a-z0-9][a-z0-9_-]{0,58}$/.test(payload.member)) { error.textContent = "Operator, reason, and synthetic names starting with test- are required."; return; }
    payload.request_id = commandKey(form, payload); const token = state.token, revision=++proposalRevision;
    button.disabled = true; error.textContent = "";
    try {
      await api("/v1/admin/test-executions", { method: "POST", body: JSON.stringify(payload) });
      if (token !== state.token || revision!==proposalRevision) return;
      state.actor = payload.actor; sessionStorage.setItem("wardstone.operatorActor", payload.actor);
      form.commandFingerprint = ""; byId("test-reason").value = "";
      await loadDashboard({ quiet: true });
      showToast("Test action proposed. Review its exact arguments in the approval queue.");
    } catch (failure) { if (token === state.token && revision===proposalRevision) error.textContent = failure.message + " Retry unchanged input to safely check the same request."; }
    finally { if (token === state.token && revision===proposalRevision) button.disabled = false; }
  }

  function commandKey(form, payload) {
    const fingerprint = JSON.stringify(payload, (_key, value) => value && typeof value === "object" && !Array.isArray(value) ? Object.fromEntries(Object.entries(value).sort(([a], [b]) => a.localeCompare(b))) : value);
    if (form.commandFingerprint !== fingerprint) {
      form.commandFingerprint = fingerprint;
      form.commandRequestID = globalThis.crypto?.randomUUID?.() || `request-${Date.now()}-${Math.random().toString(36).slice(2)}`;
    }
    return form.commandRequestID;
  }

  async function createOperatorCase(event) {
    event.preventDefault();
    const form = byId("create-case-form"), button = byId("case-create-submit"), error = byId("case-create-error");
    if (button.disabled) return;
    const actor = byId("case-actor").value.trim();
    let fields = {};
    try {
      fields = JSON.parse(byId("case-fields").value || "{}");
      if (!fields || typeof fields !== "object" || Array.isArray(fields) || Object.values(fields).some((values) => !Array.isArray(values) || values.some((value) => typeof value !== "string"))) throw new Error();
      if ((byId("case-vendor").value.trim() && Object.hasOwn(fields, "Vendor")) || (byId("case-topic").value.trim() && Object.hasOwn(fields, "Knowledge topic"))) throw new Error();
    } catch (_) { error.textContent = "Additional fields must be a JSON object mapping names to arrays of strings. Set vendor and knowledge topic in only one place."; return; }
    if (byId("case-vendor").value.trim()) fields.Vendor = [byId("case-vendor").value.trim()];
    if (byId("case-topic").value.trim()) fields["Knowledge topic"] = [byId("case-topic").value.trim()];
    const payload = { actor, reason: byId("case-reason").value.trim(), summary: byId("case-summary").value.trim(), description: byId("case-description").value, metadata: { request_type: byId("case-request-type").value.trim(), fields } };
    if (!payload.actor || !payload.reason || !payload.summary) { error.textContent = "Operator name, summary, and reason are required."; return; }
    payload.request_id = commandKey(form, payload);
    const token = state.token, revision=++createCaseRevision, openedAt=detailRevision;
    error.textContent = ""; button.disabled = true;
    try {
      const result = await api("/v1/admin/cases", { method: "POST", body: JSON.stringify(payload) });
      if (state.token !== token || revision!==createCaseRevision) return;
      state.actor = actor; sessionStorage.setItem("wardstone.operatorActor", actor);
      form.commandFingerprint = "";
      for (const [id,value] of [["case-summary",payload.summary],["case-description",payload.description],["case-reason",payload.reason]]) if(byId(id).value.trim()===value.trim()) byId(id).value="";
      await loadDashboard({ quiet: true });
      if(openedAt===detailRevision && revision===createCaseRevision) await showInvestigation(result.investigation_id);
    } catch (failure) { if (state.token === token && revision===createCaseRevision) error.textContent = failure.message + " Retry unchanged input to safely check the same request."; }
    finally { if (state.token === token && revision===createCaseRevision) button.disabled = false; }
  }

  function caseControls(investigation, payload, id) {
    const card = node("section", { className: "report-card" });
    if (!Array.isArray(payload.attempts)) return card;
    const ticket = payload.ticket || {};
    card.append(node("h3", { text: "Case and attempts" }), detailField("Origin", ticket.source || "Unknown"), node("p", { className: "detail-copy", text: ticket.summary || "" }));
    const attempts = node("div", { className: "feature-list" });
    for (const attempt of payload.attempts) {
      const button = node("button", { className: "button ghost", type: "button", text: `${attempt.id === id ? "Current · " : ""}${attempt.status} · ${attempt.id}` });
      button.disabled = attempt.id === id;
      button.addEventListener("click", () => showInvestigation(attempt.id));
      attempts.append(button);
    }
    card.append(attempts);
    const child = payload.attempts.some((attempt) => attempt.parent_id === id);
    let operation = "", label = "";
    if (["PENDING", "RUNNING", "WAITING_ON_REQUESTER"].includes(investigation.status)) { operation = "cancel"; label = "Cancel investigation"; }
    else if (["FAILED", "CANCELLED"].includes(investigation.status) && !child && payload.runs_enabled) { operation = "retry"; label = "Retry as a new attempt"; }
    const addCommand = (op, buttonLabel, reply = false) => {
      const form = node("form", { className: "action-detail case-command" });
      const actor = node("input", { value: state.actor, maxLength: 256, required: true, placeholder: "Operator name", "aria-label": "Operator name" });
      const reason = node("input", { maxLength: 1000, required: true, placeholder: "Reason", "aria-label": "Reason" });
      const body = node("textarea", { rows: 3, maxLength: 16000, required: true, "aria-label": "Reply to investigation" });
      const error = node("p", { className: "form-error", role: "alert" });
      const submit = node("button", { className: "button", type: "submit", text: buttonLabel });
      form.append(actor, reason);
      if (reply) form.append(node("label", { text: "Reply to the outstanding question" }), body);
      form.append(error, submit);
      form.addEventListener("submit", async (event) => {
        event.preventDefault(); if (submit.disabled) return;
        const command = { actor: actor.value.trim(), reason: reason.value.trim() };
        if (reply) command.body = body.value;
        if (!command.actor || !command.reason || (reply && !command.body.trim())) { error.textContent = "Operator name, reason, and any required reply are required."; return; }
        command.request_id = commandKey(form, command);
        const revision = detailRevision, token = state.token;
        submit.disabled = true; error.textContent = "";
        try {
          const result = await api(`/v1/admin/investigations/${encodeURIComponent(id)}/${op}`, { method: "POST", body: JSON.stringify(command) });
          if (detailRevision !== revision || state.token !== token) return;
          state.actor = command.actor; sessionStorage.setItem("wardstone.operatorActor", command.actor);
          await showInvestigation(result.investigation_id || id); loadDashboard({ quiet: true });
        } catch (failure) { if (detailRevision === revision && state.token === token) error.textContent = failure.message + " Retry unchanged input to safely check the same request."; }
        finally { submit.disabled = false; }
      });
      card.append(form);
    };
    if (operation) {
      card.append(node("p", { className: "detail-note", text: operation === "cancel" ? "Cancellation stops investigation writes and unsent messages. Already claimed sends may still finish; their recorded outcomes remain available." : "A retry uses current configuration and fresh evidence. Previous attempts, drafts, and audit history remain available." }));
      addCommand(operation, label);
    }
    if (ticket.source === "operator" && investigation.status === "WAITING_ON_REQUESTER" && payload.runs_enabled) addCommand("reply", "Submit reply and continue", true);
    return card;
  }

  function evidenceIndex(events) {
    const evidence = new Map();
    for (const event of events) {
      if (event.type === "evidence.collected" && event.data?.evidence_id) evidence.set(event.data.evidence_id, event);
    }
    return evidence;
  }

  function observationFreshness(value) {
    const observed = new Date(value).getTime();
    if (!value || !Number.isFinite(observed) || observed <= 0) return "Observation time unknown";
    const age = Date.now() - observed;
    if (age < -60000) return "Future-dated observation";
    if (age > 15 * 60000) return "Stale observation (at display time)";
    return "Recent observation (at display time)";
  }

  function evidenceCitation(id, evidence) {
    const citation = node("div", { className: "evidence-citation" }, [node("code", { text: id || "Evidence ID not recorded" })]);
    const event = evidence.get(id);
    if (!event) {
      citation.append(node("small", { text: "Provenance and observation time not recorded in timeline." }));
      return citation;
    }
    const data = event.data || {};
    citation.append(node("small", { text: `Source: ${event.actor_type === "CONNECTOR" ? event.actor_id || "unknown" : "unknown"} · Capability: ${data.capability || "not recorded"} · Kind: ${data.kind || "not recorded"}` }), node("small", { text: `Observed: ${data.observed_at || "not recorded"} · ${observationFreshness(data.observed_at)}` }));
    if (data.title) citation.append(node("small", { text: `Document: ${data.title}` }));
    if (data.format === "pdf") citation.append(node("small", { text: `PDF${Number.isInteger(data.page_number) && data.page_number > 0 ? ` · Page ${data.page_number} of ${data.page_count || "unknown"}` : ""}${data.extraction_method ? ` · ${data.extraction_method}` : ""}` }));
    if (data.kind === "research.document_unavailable") citation.append(node("small", { text: `Evidence gap: ${humanize(data.status || "unavailable")} · Operator review required` }));
    if (Array.isArray(data.affected_pages) && data.affected_pages.length) citation.append(node("small", { text: `Affected pages: ${data.affected_pages.join(", ")}` }));
    if (data.source_url) citation.append(node("small", { text: `Source URL: ${data.source_url}` }));
    if (data.content_sha256) citation.append(node("small", { text: `${data.format === "pdf" ? "Original file" : "Content"} SHA-256: ${data.content_sha256}` }));
    if (data.claim_type) citation.append(node("small", { text: `Evidence type: ${data.claim_type}${data.truncated ? " · excerpt truncated" : ""}` }));
    return citation;
  }

  function reportCard(report, evidence) {
    const card = node("section", { className: "report-card" }, [node("h3", { text: "Investigation report" })]);
    if (!report) {
      card.append(node("p", { className: "detail-copy", text: "No structured report was recorded for this investigation." }));
      return card;
    }
    card.append(node("small", { className: "detail-copy", text: `Report version ${report.version}` }), node("p", { className: "report-summary", text: report.summary }));
    if (report.risk_assessment) {
      card.append(node("h4", { text: "Provisional vendor risk" }), detailField("Risk rating", report.risk_assessment.rating), node("p", { className: "detail-copy", text: report.risk_assessment.rationale }));
      for (const id of report.risk_assessment.evidence_ids || []) card.append(evidenceCitation(id, evidence));
      const scoring = report.risk_assessment.scoring;
      if (scoring) {
        card.append(detailField("Scoring method", "Go · " + scoring.method), detailField("Rubric", scoring.context?.rubric?.version || "Not configured"), detailField("Rubric SHA-256", scoring.rubric_digest), detailField("Risk points", Number.isInteger(scoring.score) ? `${scoring.score}/100` : "Withheld — incomplete review"));
        const context = scoring.context || {};
        card.append(node("h4", { text: "Administrator deployment context" }), detailField("Vendor", context.vendor), detailField("Use case", context.deployment?.use_case), detailField("Data classification", context.deployment?.data_classification), detailField("Access scope", context.deployment?.access_scope));
        if (context.rubric) card.append(node("p", { className: "detail-note", text: `Thresholds: MODERATE at ${context.rubric.moderate_at}, HIGH at ${context.rubric.high_at}. Met adds no risk points, partial adds half the weight, and unmet adds the full weight. Any evidence gap withholds the score.` }));
        card.append(node("h4", { text: "Criterion assessments — model interpretation" }));
        for (const assessment of scoring.assessment?.criteria || []) {
          const criterion = context.rubric?.criteria?.find((c) => c.id === assessment.id);
          const item = node("article", { className: "action-detail" }, [node("h4", { text: assessment.id }), detailField("Status", assessment.status), detailField("Weight", `${criterion?.weight ?? "Unknown"}/100`), detailField("Required evidence", criterion?.evidence), node("p", { className: "detail-copy", text: criterion?.requirement || "" }), node("p", { className: "detail-copy", text: assessment.reason })]);
          for (const id of assessment.evidence_ids || []) item.append(evidenceCitation(id, evidence));
          card.append(item);
        }
        card.append(node("h4", { text: "Evidence gaps" }));
        for (const gap of scoring.gaps || []) {
          const item = node("article", { className: "action-detail" }, [node("strong", { text: humanize(gap.code) }), node("p", { className: "detail-copy", text: gap.detail })]);
          for (const id of gap.evidence_ids || []) item.append(evidenceCitation(id, evidence));
          card.append(item);
        }
        if (!scoring.gaps?.length) card.append(node("p", { className: "detail-copy", text: "No rubric gaps recorded. The assessment still requires operator review." }));
        card.append(node("p", { className: "detail-note", text: "Go applies the stored rubric to cited model interpretations. This provisional rating does not independently verify controls or approve the vendor." }));
      } else card.append(node("p", { className: "detail-note", text: "Legacy model-assigned rating. No deterministic rubric was recorded for this report." }));
    }
    for (const [kind, label] of [["verified", report.risk_assessment?.scoring ? "Evidence-backed findings" : "Verified findings"], ["vendor_claim", "Vendor claims — not independently verified"], ["assumption", "Assumptions"]]) {
      const findings = (report.findings || []).filter((finding) => finding.kind === kind);
      const list = node("ul", { className: `finding-list ${kind}` });
      for (const finding of findings) {
        const item = node("li", {}, [node("p", { className: "detail-copy", text: finding.statement })]);
        for (const id of finding.evidence_ids || []) item.append(evidenceCitation(id, evidence));
        if (!finding.evidence_ids?.length) item.append(node("small", { className: "detail-copy", text: "No evidence cited." }));
        list.append(item);
      }
      card.append(node("h4", { text: label }), findings.length ? list : node("p", { className: "detail-copy", text: "None recorded." }));
    }
    card.append(node("h4", { text: "Unknowns" }), report.unknowns?.length ? node("ul", { className: "unknown-list" }, report.unknowns.map((unknown) => node("li", { className: "detail-copy", text: unknown }))) : node("p", { className: "detail-copy", text: "None recorded." }));
    for (const [label, value] of [["Escalation reason", report.escalation_reason], ["Expected impact", report.expected_impact], ["Requester summary draft", report.requester_summary]]) {
      card.append(node("h4", { text: label }), node("p", { className: "detail-copy", text: value || "Not provided." }));
    }
    card.append(node("p", { className: "detail-note", text: "Observation age is shown at display time. Prior-run evidence is context only; a recent timestamp alone does not establish current-run mutation eligibility." }));
    return card;
  }

  function actionsCard(events, evidence) {
    const card = node("section", { className: "actions-card" }, [node("h3", { text: "Proposed actions and policy" })]);
    const proposals = events.filter((event) => event.type === "action.proposed");
    if (!proposals.length) card.append(node("p", { className: "detail-copy", text: "No proposed actions recorded." }));
    for (const event of proposals) {
      const data = event.data || {};
      const action = node("article", { className: "action-detail" }, [node("h4", { text: data.capability || "Capability not recorded" }), detailField("Action ID", data.action_id), node("p", { className: "detail-copy", text: data.reason || "No proposal reason recorded." }), node("h4", { text: "Exact arguments (JSON)" }), node("pre", { className: "literal-json", text: JSON.stringify(data.arguments ?? null, null, 2) })]);
      if (data.digest) action.append(detailField("Action digest", data.digest));
      action.append(node("h4", { text: "Evidence citations" }));
      for (const id of data.evidence_ids || []) action.append(evidenceCitation(id, evidence));
      if (!data.evidence_ids?.length) action.append(node("p", { className: "detail-copy", text: "No evidence citations recorded." }));
      const policies = events.filter((item) => item.type === "policy.evaluated" && data.action_id && item.data?.action_id === data.action_id);
      action.append(node("h4", { text: "Policy evaluation" }));
      for (const policy of policies) action.append(node("p", { className: "detail-copy policy-result", text: `${policy.data.decision || "Unknown decision"} · ${policy.data.reason || "No policy reason recorded."}` }));
      if (!policies.length) action.append(node("p", { className: "detail-copy", text: "Policy evaluation not recorded." }));
      card.append(action);
    }
    return card;
  }

  function conversationCard(payload, investigationID, status, origin) {
    const internal=origin==="operator"||(payload.messages||[]).some((m)=>m.source==="operator");
    const card = node("section", { className: "conversation-card" }, [node("h3", { text: internal ? "Internal case conversation" : "Requester conversation" }), node("p", { className: "detail-note", text: internal ? "This conversation stays in the console. Reply to outstanding questions using the case controls above." : "Outbound messages are drafts in SHADOW mode. In REVIEW mode, an operator approves the exact message to send. Delivery status is recorded separately." })]);
    const refresh = node("button", { type: "button", className: "button button-secondary", text: internal ? "Refresh conversation" : "Refresh delivery status" });
    refresh.addEventListener("click", () => showInvestigation(investigationID));
    card.append(refresh);
    if (payload.error) card.append(node("p", { className: "detail-copy", text: payload.error }));
    else if (!payload.messages?.length) card.append(node("p", { className: "detail-copy", text: "No messages recorded." }));
    for (const message of payload.messages || []) {
      const article = node("article", { className: "message-detail" }, [node("h4", { text: message.direction === "OUTBOUND" ? "Wardstone outbound / draft" : message.direction === "INBOUND" ? (message.source === "operator" ? "Operator reply" : "Requester reply") : "Message" }), node("small", { className: "detail-copy", text: `${formatDate(message.created_at)} · ${message.author || "Unknown author"} · ${message.source || "Unknown source"}` }), node("p", { className: "detail-copy", text: message.body })]);
      const delivery = (payload.deliveries || []).find((item) => item.message_id === message.id);
      if (delivery) {
        article.append(detailField("Delivery", humanize(delivery.status)), detailField("Send attempts", String(delivery.attempt || 0)), detailField("Reviewed by", delivery.reviewed_by), detailField("Remote comment", delivery.remote_id));
        if (delivery.reviewed_body) article.append(node("h4", { text: "Approved message" }), node("p", { className: "detail-copy", text: delivery.reviewed_body }));
        if (delivery.last_error) article.append(node("p", { className: "detail-note", text: `Last send error: ${delivery.last_error}` }));
        if (delivery.status === "SUPERSEDED") article.append(node("p", { className: "detail-note", text: "A newer message or case state superseded this draft before sending. It cannot be resent." }));
        if (message.source === "jira" && payload.recovery_enabled && delivery.reviewed_at && ["NEEDS_RECONCILIATION", "FAILED"].includes(delivery.status)) {
          const canResend = message.id === payload.messages.at(-1)?.id && ["WAITING_ON_REQUESTER", "COMPLETED"].includes(status);
          article.append(recoveryForm(investigationID, message.id, delivery, canResend));
        }
      } else if (message.source === "jira" && payload.review_enabled && message.direction === "OUTBOUND" && message.id === payload.messages.at(-1)?.id && ["WAITING_ON_REQUESTER", "COMPLETED"].includes(status)) {
        const body = node("textarea", { id: `review-body-${message.id}`, rows: 6, maxLength: 16000, required: true, value: message.body });
        const actor = node("input", { id: `review-actor-${message.id}`, required: true, maxLength: 256, value: state.actor, placeholder: "you@example.com" });
        const button = node("button", { className: "button", type: "submit", text: "Approve and send to Jira" });
        const error = node("p", { className: "form-error", role: "alert" });
        const form = node("form", { className: "message-review" }, [node("label", { htmlFor: body.id, text: "Exact message to send" }), body, node("label", { htmlFor: actor.id, text: "Operator identity" }), actor, node("p", { className: "detail-note", text: "Submitting approves this exact text for Jira delivery and records your identity. Infrastructure changes remain disabled." }), button, error]);
        form.addEventListener("submit", async (event) => {
          event.preventDefault();
          const token = state.token;
          button.disabled = true;
          error.textContent = "";
          try {
            await api(`/v1/investigations/${encodeURIComponent(investigationID)}/messages/${encodeURIComponent(message.id)}/review`, { method: "POST", body: JSON.stringify({ body: body.value, actor: actor.value.trim(), draft_digest: payload.draft_digests?.[message.id] }) });
            if (state.token !== token) return;
            state.actor = actor.value.trim();
            sessionStorage.setItem("wardstone.operatorActor", state.actor);
            showToast("Reviewed message queued for Jira delivery.");
            await showInvestigation(investigationID);
          } catch (failure) { if (state.token === token) error.textContent = failure.message; }
          finally { button.disabled = false; }
        });
        article.append(form);
      }
      card.append(article);
    }
    return card;
  }

  function recoveryForm(investigationID, messageID, delivery, canResend) {
    const revision = detailRevision;
    const actor = node("input", { id: `recovery-actor-${messageID}`, required: true, maxLength: 256, value: state.actor, placeholder: "you@example.com" });
    const reason = node("textarea", { id: `recovery-reason-${messageID}`, required: true, rows: 2, maxLength: 2000 });
    const remote = node("input", { id: `recovery-comment-${messageID}`, maxLength: 128, placeholder: "Jira comment ID" });
    const verify = node("button", { type: "submit", className: "button button-secondary", text: "Verify Jira comment" });
    const acknowledge = node("input", { id: `recovery-risk-${messageID}`, type: "checkbox" });
    const resend = node("button", { type: "button", className: "button", text: "Resend approved message" });
    const error = node("p", { className: "form-error", role: "alert" });
    const form = node("form", { className: "message-recovery" }, [
      node("h4", { text: "Recover Jira delivery" }),
      node("p", { className: "detail-note", text: "Check Jira for the approved message and enter its comment ID. Wardstone verifies the issue, bot author, creation time, and exact message. A missing or mismatched comment leaves delivery uncertain." }),
      node("label", { htmlFor: actor.id, text: "Operator identity" }), actor,
      node("label", { htmlFor: reason.id, text: "Reason for recovery" }), reason,
      node("label", { htmlFor: remote.id, text: "Jira comment ID to verify" }), remote, verify,
    ]);
    if (canResend) form.append(node("label", { htmlFor: acknowledge.id, className: "recovery-risk" }, [acknowledge, node("span", { text: "I checked Jira and accept that resending may create a duplicate comment." })]), resend);
    else form.append(node("p", { className: "detail-note", text: "This message has been superseded. You can verify a past delivery, but cannot resend it." }));
    form.append(error);
    let busy = false;
    async function recover(action) {
      if (busy) return;
      error.textContent = "";
      if (!actor.value.trim() || !reason.value.trim()) { error.textContent = "Enter your identity and a reason for recovery."; return; }
      if (action === "reconcile" && !/^[A-Za-z0-9_-]{1,128}$/.test(remote.value.trim())) { error.textContent = "Enter the Jira comment ID to verify."; return; }
      if (action === "resend" && !acknowledge.checked) { error.textContent = "Acknowledge the risk of a duplicate comment before resending."; return; }
      const token = state.token;
      busy = true;
      verify.disabled = resend.disabled = true;
      try {
        const request = { action, actor: actor.value.trim(), reason: reason.value.trim(), revision: delivery.revision };
        if (action === "reconcile") request.remote_id = remote.value.trim();
        else request.acknowledge_duplicate_risk = true;
        const result = await api(`/v1/investigations/${encodeURIComponent(investigationID)}/messages/${encodeURIComponent(messageID)}/recovery`, { method: "POST", body: JSON.stringify(request) });
        if (state.token !== token || revision !== detailRevision || !byId("investigation-dialog").open) return;
        state.actor = actor.value.trim();
        sessionStorage.setItem("wardstone.operatorActor", state.actor);
        const outcomes = {
          verified: "Jira comment verified. Delivery recorded as sent.",
          resend_queued: "The exact approved message is queued for resend.",
          comment_not_found: "Jira comment not found. Delivery remains unresolved; no message was sent.",
          comment_mismatch: "Jira comment did not match. Delivery remains unresolved; no message was sent.",
          jira_verification_unavailable: "Jira verification unavailable. Delivery remains unresolved; no message was sent.",
        };
        await showInvestigation(investigationID);
        if (state.token === token && detailRevision === revision + 1) {
          const notice = node("p", { className: "detail-note", role: "status", text: outcomes[result.outcome] || "Delivery recovery recorded." });
          byId("dialog-body").append(notice);
          showToast(notice.textContent);
        }
      } catch (failure) { if (state.token === token && revision === detailRevision) error.textContent = failure.message; }
      finally { busy = false; verify.disabled = resend.disabled = false; }
    }
    form.addEventListener("submit", (event) => { event.preventDefault(); return recover("reconcile"); });
    resend.addEventListener("click", () => recover("resend"));
    return form;
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
    if (event.type === "action.proposed") return [data.action_id && `Action: ${data.action_id}`, data.capability, data.reason].filter(Boolean).join(" · ");
    if (event.type === "policy.evaluated") return [data.action_id && `Action: ${data.action_id}`, data.decision, data.reason].filter(Boolean).join(" · ");
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
    dashboardRevision++;createCaseRevision++;proposalRevision++;
    byId("case-create-submit").disabled=false;byId("test-proposal-submit").disabled=false;
    detailRevision++;
    setupRevision++;
    testExecutionRevision++;
    byId("test-execution").hidden = true;
    byId("test-execution-list").replaceChildren();
    activeSetupProbe = "";
    state.token = "";
    state.investigations = [];
    state.approvals = [];
    state.setup = null;
    sessionStorage.removeItem("wardstone.operatorToken");
    tokenInput.value = "";
    connection.hidden = false;
    dashboard.hidden = true;
    sessionButton.textContent = "Connect";
    refreshButton.disabled = true;
    byId("routing-preview-result").replaceChildren();
    byId("routing-preview-error").textContent = "";
    byId("setup-list").replaceChildren();
    byId("setup-features").replaceChildren();
    byId("setup-error").textContent = "";
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
  byId("setup-list").addEventListener("click", (event) => {
    const button = event.target.closest("button[data-setup-name]");
    if (button) testSetupConnection(button);
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

  byId("test-execution-refresh").addEventListener("click", loadTestExecutions);
  byId("test-proposal-form").addEventListener("submit", proposeTestExecution);
  byId("test-actor").value = state.actor;
  byId("create-case-form").addEventListener("submit", createOperatorCase);
  byId("case-actor").value = state.actor;

  checkHealth();
  setInterval(checkHealth, 30000);
  setInterval(() => { if (!document.hidden && state.token) loadDashboard({ quiet: true }); }, 30000);
  if (state.token) loadDashboard();
})();
