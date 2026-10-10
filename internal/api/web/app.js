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
      loadSetup({ quiet });
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
      const [investigation, timelinePayload, messagesPayload] = await Promise.all([
        api(`/v1/investigations/${encodeURIComponent(id)}`),
        api(`/v1/investigations/${encodeURIComponent(id)}/timeline`),
        api(`/v1/investigations/${encodeURIComponent(id)}/messages`).catch((error) => {
          if (error.status === 401) throw error;
          return { error: "Conversation could not be loaded." };
        }),
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
        if (event.type === "action.proposed") entry.append(node("pre", { className: "literal-json", text: JSON.stringify(event.data?.arguments ?? null, null, 2) }));
        timeline.append(entry);
      }
      body.replaceChildren(fields, routingCard(investigation.routing), reportCard(investigation.work_package, evidence), node("h3", { text: investigation.failure ? "Failure" : "Diagnosis" }), node("p", { className: "diagnosis", text: investigation.failure || investigation.diagnosis || "No diagnosis yet." }), actionsCard(events, evidence), conversationCard(messagesPayload, id, investigation.status), node("h3", { text: "Audit timeline" }), timeline);
    } catch (error) {
      if (state.token !== sessionToken || revision !== detailRevision) return;
      body.replaceChildren(emptyState("!", "Could not load investigation", error.message));
    }
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
    if (data.source_url) citation.append(node("small", { text: `Source URL: ${data.source_url}` }));
    if (data.content_sha256) citation.append(node("small", { text: `Content SHA-256: ${data.content_sha256}` }));
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
    }
    for (const [kind, label] of [["verified", "Verified findings"], ["assumption", "Assumptions"]]) {
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

  function conversationCard(payload, investigationID, status) {
    const card = node("section", { className: "conversation-card" }, [node("h3", { text: "Requester conversation" }), node("p", { className: "detail-note", text: "Outbound messages are drafts in SHADOW mode. In REVIEW mode, an operator approves the exact message to send. Delivery status is recorded separately." })]);
    if (payload.error) card.append(node("p", { className: "detail-copy", text: payload.error }));
    else if (!payload.messages?.length) card.append(node("p", { className: "detail-copy", text: "No messages recorded." }));
    for (const message of payload.messages || []) {
      const article = node("article", { className: "message-detail" }, [node("h4", { text: message.direction === "OUTBOUND" ? "Wardstone outbound / draft" : message.direction === "INBOUND" ? "Requester reply" : "Message" }), node("small", { className: "detail-copy", text: `${formatDate(message.created_at)} · ${message.author || "Unknown author"} · ${message.source || "Unknown source"}` }), node("p", { className: "detail-copy", text: message.body })]);
      const delivery = (payload.deliveries || []).find((item) => item.message_id === message.id);
      if (delivery) {
        article.append(detailField("Delivery", delivery.status), detailField("Reviewed by", delivery.reviewed_by), detailField("Remote comment", delivery.remote_id));
        if (delivery.reviewed_body) article.append(node("h4", { text: "Approved message" }), node("p", { className: "detail-copy", text: delivery.reviewed_body }));
        if (delivery.last_error) article.append(node("p", { className: "form-error", text: delivery.last_error }));
      } else if (payload.review_enabled && message.direction === "OUTBOUND" && message.id === payload.messages.at(-1)?.id && ["WAITING_ON_REQUESTER", "COMPLETED"].includes(status)) {
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
    detailRevision++;
    setupRevision++;
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

  checkHealth();
  setInterval(checkHealth, 30000);
  setInterval(() => { if (!document.hidden && state.token) loadDashboard({ quiet: true }); }, 30000);
  if (state.token) loadDashboard();
})();
