"use strict";

// Exercise the shipped browser script with a minimal DOM. String-to-HTML sinks
// deliberately throw so ticket/model/audit text cannot silently become markup.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

class Element {
  constructor(tag = "div") {
    this.tagName = tag;
    this.children = [];
    this.textContent = "";
    this.value = "";
    this.hidden = false;
    this.listeners = {};
    this.classList = { add() {}, remove() {} };
  }
  set innerHTML(_) { throw new Error("Unsafe HTML rendering"); }
  set outerHTML(_) { throw new Error("Unsafe HTML rendering"); }
  insertAdjacentHTML() { throw new Error("Unsafe HTML rendering"); }
  append(...children) { this.children.push(...children); }
  replaceChildren(...children) { this.children = children; }
  setAttribute(key, value) { this[key] = value; }
  addEventListener(name, callback) { this.listeners[name] = callback; }
  showModal() { this.open = true; }
  close() { this.open = false; }
  focus() {}
}

function harness(responses = {}) {
  const elements = new Map();
  const requests = [];
  const get = (id) => {
    if (!elements.has(id)) elements.set(id, new Element());
    return elements.get(id);
  };
  const context = {
    document: { getElementById: get, createElement: (tag) => new Element(tag), querySelectorAll: () => [], addEventListener() {}, hidden: false },
    sessionStorage: { getItem: () => "", setItem() {}, removeItem() {} },
    Headers, AbortController, setInterval() {}, setTimeout() {}, clearTimeout() {},
    fetch: async (url, options = {}) => {
      requests.push({ url, options });
      const configured = responses[url] || {};
      const result = typeof configured === "function" ? await configured(url, options) : configured;
      return { ok: result.status == null || result.status < 400, status: result.status || 200, json: async () => result.body || {} };
    },
  };
  vm.runInNewContext(fs.readFileSync(path.join(__dirname, "web/app.js"), "utf8"), context);
  return { get, requests };
}

function textOf(element) {
  return [element.textContent, ...element.children.map(textOf)].join("\n");
}

function descendants(element) {
  return [element, ...element.children.flatMap(descendants)];
}

function openInvestigation(get, id) {
  get("investigation-list").listeners.click({ target: { closest: () => ({ dataset: { investigationId: id } }) } });
}

const submit = (element) => element.listeners.submit({ preventDefault() {} });
const settle = () => new Promise((resolve) => setImmediate(resolve));

test("routing preview renders untrusted explanations as literal text", async () => {
  const hostile = '<img src=x onerror="alert(1)">';
  const { get, requests } = harness({
    "/v1/admin/routing/preview": { body: { routing: { specialist: "vendor_review", classification: "vendor_review", source: "rule", rule_name: hostile, reason: hostile } } },
  });
  get("routing-payload").value = '{"external_id":"TEST-1","summary":"Review vendor"}';
  await submit(get("routing-preview-form"));
  const text = textOf(get("routing-preview-result"));
  assert.ok(text.includes(hostile));
  assert.ok(text.includes("model intent classification is unnecessary"));
  assert.equal(get("routing-preview-submit").disabled, false);
  const request = requests.find((request) => request.url === "/v1/admin/routing/preview");
  assert.equal(request.options.method, "POST");
  assert.equal(JSON.parse(request.options.body).external_id, "TEST-1");
  assert.equal(request.options.headers.get("Content-Type"), "application/json");
});

test("routing preview rejects malformed JSON and clears obsolete results", async () => {
  const { get, requests } = harness();
  get("routing-preview-result").append(new Element());
  get("routing-payload").value = "{invalid";
  await submit(get("routing-preview-form"));
  assert.equal(get("routing-preview-result").children.length, 0);
  assert.match(get("routing-preview-error").textContent, /valid JSON/);
  assert.equal(requests.filter((request) => request.url === "/v1/admin/routing/preview").length, 0);
  assert.equal(get("routing-preview-submit").disabled, false);
});

test("fallback preview explains that model classification has not run", async () => {
  const { get } = harness({
    "/v1/admin/routing/preview": { body: { routing: { specialist: "help_desk", classification: "unclassified", source: "fallback", fallback_code: "model_required", reason: "Intent fallback is enabled." } } },
  });
  get("routing-payload").value = '{"external_id":"TEST-1","summary":"Help"}';
  await submit(get("routing-preview-form"));
  const text = textOf(get("routing-preview-result"));
  assert.ok(text.includes("model_required"));
  assert.ok(text.includes("preview stops before model intent classification"));
});

test("investigation shows initial route and later handoff with literal audit text", async () => {
  const hostile = "<script>document.body.remove()</script>";
  const { get } = harness({
    "/v1/investigations/inv-1": { body: { id: "inv-1", specialist: "access_management", status: "RUNNING", routing: { specialist: "help_desk", classification: "unclassified", source: "fallback", reason: hostile } } },
    "/v1/investigations/inv-1/timeline": { body: { events: [
      { type: "dispatcher.routed", data: { specialist: "help_desk", source: "fallback", reason: hostile } },
      { type: "specialist.handed_off", data: { from: "help_desk", to: "access_management", reason: hostile } },
    ] } },
  });
  get("investigation-list").listeners.click({ target: { closest: () => ({ dataset: { investigationId: "inv-1" } }) } });
  await settle();
  const text = textOf(get("dialog-body"));
  for (const expected of ["Current specialist", "access_management", "Initial ticket routing", "help_desk → access_management", hostile]) assert.ok(text.includes(expected), `Missing ${expected}`);
});

test("legacy investigations show routing data as absent", async () => {
  const { get } = harness({ "/v1/investigations/inv-legacy": { body: { id: "inv-legacy", specialist: "help_desk", status: "COMPLETED" } } });
  get("investigation-list").listeners.click({ target: { closest: () => ({ dataset: { investigationId: "inv-legacy" } }) } });
  await settle();
  assert.ok(textOf(get("dialog-body")).includes("Routing details were not recorded"));
  assert.ok(textOf(get("dialog-body")).includes("No structured report was recorded"));
  assert.ok(textOf(get("dialog-body")).includes("No proposed actions recorded"));
  assert.ok(textOf(get("dialog-body")).includes("No messages recorded"));
});

test("report separates findings, assumptions and unknowns with literal citations and provenance", async () => {
  const hostile = '<img src=x onerror="alert(1)">';
  const citation = `evidence-${hostile}`;
  const observed = new Date(Date.now() - 60000).toISOString();
  const report = {
    version: 1, summary: `Summary ${hostile}`,
    findings: [
      { kind: "verified", statement: `Direct membership ${hostile}`, evidence_ids: [citation] },
      { kind: "assumption", statement: `Business need ${hostile}`, evidence_ids: ["javascript:alert(1)"] },
    ],
    unknowns: [`Effective access ${hostile}`], escalation_reason: `Review ${hostile}`,
    requester_summary: `Requester draft ${hostile}`, expected_impact: `Impact ${hostile}`,
  };
  const { get, requests } = harness({
    "/v1/investigations/report-1": { body: { status: "COMPLETED", work_package: report } },
    "/v1/investigations/report-1/timeline": { body: { events: [
      { type: "evidence.collected", actor_type: "CONNECTOR", actor_id: `google-${hostile}`, data: { evidence_id: citation, kind: `google.access_request-${hostile}`, capability: `google.access.inspect-${hostile}`, observed_at: observed } },
    ] } },
  });
  openInvestigation(get, "report-1");
  await settle();
  const body = get("dialog-body");
  const nodes = descendants(body);
  const verified = nodes.find((item) => item.className === "finding-list verified");
  const assumptions = nodes.find((item) => item.className === "finding-list assumption");
  assert.ok(textOf(verified).includes(report.findings[0].statement));
  assert.ok(!textOf(verified).includes(report.findings[1].statement));
  assert.ok(textOf(assumptions).includes(report.findings[1].statement));
  assert.ok(textOf(assumptions).includes("javascript:alert(1)"));
  assert.ok(textOf(assumptions).includes("Provenance and observation time not recorded"));
  const text = textOf(body);
  for (const value of [report.summary, ...report.unknowns, report.escalation_reason, report.requester_summary, report.expected_impact, citation, observed, `Source: google-${hostile}`, `Capability: google.access.inspect-${hostile}`, "Recent observation (at display time)", "current-run mutation eligibility"]) assert.ok(text.includes(value), `Missing ${value}`);
  assert.ok(nodes.some((item) => item.tagName === "code" && item.textContent === citation));
  assert.ok(!nodes.some((item) => ["a", "img", "script", "iframe"].includes(item.tagName)));
  assert.deepEqual(requests.filter((item) => item.url.startsWith("/v1/investigations/")).map((item) => item.url).sort(), ["/v1/investigations/report-1", "/v1/investigations/report-1/messages", "/v1/investigations/report-1/timeline"]);
});

test("citations expose stale, future and unknown observation times without inferring provenance", async () => {
  const { get } = harness({
    "/v1/investigations/ages": { body: { work_package: { version: 1, summary: "Review ages", findings: [{ kind: "verified", statement: "Historical context", evidence_ids: ["old", "future", "legacy"] }], unknowns: [] } } },
    "/v1/investigations/ages/timeline": { body: { events: [
      { type: "evidence.collected", actor_type: "CONNECTOR", actor_id: "google", data: { evidence_id: "old", observed_at: new Date(Date.now() - 16 * 60000).toISOString() } },
      { type: "evidence.collected", actor_type: "CONNECTOR", actor_id: "google", data: { evidence_id: "future", observed_at: new Date(Date.now() + 5 * 60000).toISOString() } },
      { type: "evidence.collected", data: { evidence_id: "legacy", kind: "google.access_request" } },
    ] } },
  });
  openInvestigation(get, "ages");
  await settle();
  const text = textOf(get("dialog-body"));
  for (const value of ["Stale observation", "Future-dated observation", "Observation time unknown", "Source: unknown · Capability: not recorded"]) assert.ok(text.includes(value), `Missing ${value}`);
});

test("exact action arguments and policy reasons render literally and join by action ID", async () => {
  const hostile = '<script>alert("policy")</script>';
  const argumentsJSON = { group_email: "engineering@example.com", user_email: "requester@example.com", note: hostile, nested: { url: "javascript:alert(1)", flag: false } };
  const { get } = harness({
    "/v1/investigations/actions/timeline": { body: { events: [
      { type: "action.proposed", data: { action_id: "grant", capability: "google.groups.add_member", arguments: argumentsJSON, reason: hostile, digest: "exact-digest", evidence_ids: ["evidence-1"] } },
      { type: "action.proposed", data: { action_id: "remove", capability: "google.groups.remove_member", arguments: { user_email: "other@example.com" } } },
      { type: "action.proposed", data: { action_id: "unevaluated", capability: "google.groups.get", arguments: {} } },
      { type: "policy.evaluated", data: { action_id: "remove", decision: "REQUIRE_APPROVAL", reason: "remove-only" } },
      { type: "policy.evaluated", data: { action_id: "grant", decision: "DENY", reason: `shadow_mode ${hostile}` } },
    ] } },
  });
  openInvestigation(get, "actions");
  await settle();
  const nodes = descendants(get("dialog-body"));
  const actions = nodes.filter((item) => item.className === "action-detail");
  assert.equal(actions.length, 3);
  const grant = textOf(actions[0]);
  for (const value of ["Exact arguments (JSON)", "exact-digest", "evidence-1", "DENY", `shadow_mode ${hostile}`]) assert.ok(grant.includes(value), `Missing ${value}`);
  assert.ok(!grant.includes("remove-only"));
  assert.ok(textOf(actions[1]).includes("REQUIRE_APPROVAL · remove-only"));
  assert.ok(!textOf(actions[1]).includes("shadow_mode"));
  assert.ok(textOf(actions[2]).includes("Policy evaluation not recorded"));
  const argumentsNode = descendants(actions[0]).find((item) => item.tagName === "pre");
  assert.deepEqual(JSON.parse(argumentsNode.textContent), argumentsJSON);
  assert.ok(argumentsNode.textContent.includes(hostile.replaceAll('"', '\\"')));
  assert.ok(!nodes.some((item) => ["a", "script"].includes(item.tagName)));
});

test("requester questions, replies and final drafts preserve literal conversation text", async () => {
  const hostile = '<a href="javascript:alert(1)">Engineering</a>';
  const { get } = harness({
    "/v1/investigations/conversation/messages": { body: { messages: [
      { direction: "OUTBOUND", author: "wardstone", source: "jira", body: `Which resource?\n${hostile}` },
      { direction: "INBOUND", author: `requester-${hostile}`, source: "jira", body: hostile },
      { direction: "OUTBOUND", author: "wardstone", source: "jira", body: `Final result\n${hostile}` },
    ] } },
  });
  openInvestigation(get, "conversation");
  await settle();
  const nodes = descendants(get("dialog-body"));
  const messages = nodes.filter((item) => item.className === "message-detail");
  assert.equal(messages.length, 3);
  assert.ok(textOf(messages[0]).includes(`Which resource?\n${hostile}`));
  assert.ok(textOf(messages[1]).includes("Requester reply"));
  assert.ok(textOf(messages[1]).includes(`requester-${hostile}`));
  assert.ok(textOf(messages[2]).includes(`Final result\n${hostile}`));
  assert.ok(textOf(get("dialog-body")).includes("Delivery status is recorded separately"));
  assert.ok(!nodes.some((item) => item.tagName === "a"));
});

test("unavailable conversation leaves the report and diagnosis readable", async () => {
  const { get } = harness({
    "/v1/investigations/partial": { body: { diagnosis: "Diagnosis retained", work_package: { version: 1, summary: "Report retained", findings: [], unknowns: [] } } },
    "/v1/investigations/partial/messages": { status: 404 },
  });
  openInvestigation(get, "partial");
  await settle();
  const text = textOf(get("dialog-body"));
  for (const value of ["Diagnosis retained", "Report retained", "Conversation could not be loaded"]) assert.ok(text.includes(value));
});

test("a slow detail response cannot replace another case's report and actions", async () => {
  let resolveOld;
  const oldResponse = new Promise((resolve) => { resolveOld = resolve; });
  const { get } = harness({
    "/v1/investigations/old": () => oldResponse,
    "/v1/investigations/new": { body: { diagnosis: "New case diagnosis" } },
  });
  openInvestigation(get, "old");
  openInvestigation(get, "new");
  await settle();
  resolveOld({ body: { diagnosis: "Old case diagnosis" } });
  await settle();
  assert.ok(textOf(get("dialog-body")).includes("New case diagnosis"));
  assert.ok(!textOf(get("dialog-body")).includes("Old case diagnosis"));
});

test("setup renders connector readiness as literal text", async () => {
  const hostile = '<img src=x onerror="alert(1)">';
  const { get, requests } = harness({
    "/v1/admin/overview": { body: { mode: "SHADOW", investigations: {}, approvals: {}, jobs: {} } },
    "/v1/admin/investigations?limit=100": { body: { investigations: [] } },
    "/v1/admin/approvals?status=PENDING": { body: { approvals: [] } },
    "/v1/admin/setup": { body: { state: "not_tested", features: { jira_intake: true }, components: [
      { name: "model", label: "Model provider", description: hostile, permission: hostile, required: true, configured: true, probeable: true, state: "not_tested", message: hostile },
      { name: "google", label: "Google Workspace", description: "Evidence", permission: "Directory read", configured: false, probeable: false, state: "not_configured", message: "Configuration is not present." },
    ] } },
  });
  get("operator-token").value = "operator-secret";
  await submit(get("connection-form"));
  await settle();
  const text = textOf(get("setup-list"));
  assert.ok(text.includes(hostile));
  assert.ok(text.includes("Model provider"));
  assert.ok(text.includes("Not Configured"));
  assert.ok(textOf(get("setup-features")).includes("Jira Intake: enabled"));
  const request = requests.find((item) => item.url === "/v1/admin/setup");
  assert.equal(request.options.headers.get("Authorization"), "Bearer operator-secret");
});

test("setup connection test uses fixed authenticated POST and updates result", async () => {
  const base = {
    "/v1/admin/overview": { body: { mode: "SHADOW", investigations: {}, approvals: {}, jobs: {} } },
    "/v1/admin/investigations?limit=100": { body: { investigations: [] } },
    "/v1/admin/approvals?status=PENDING": { body: { approvals: [] } },
    "/v1/admin/setup": { body: { state: "not_tested", features: {}, components: [
      { name: "model", label: "Model", description: "Provider", permission: "Metadata", configured: true, probeable: true, state: "not_tested", message: "Not tested." },
    ] } },
    "/v1/admin/setup/connectors/model/test": { body: {
      component: { name: "model", label: "Model", description: "Provider", permission: "Metadata", configured: true, probeable: true, state: "ready", message: "Verified." },
      setup: { state: "ready", features: {}, components: [{ name: "model", label: "Model", description: "Provider", permission: "Metadata", configured: true, probeable: true, state: "ready", message: "Verified." }] },
    } },
  };
  const { get, requests } = harness(base);
  get("operator-token").value = "operator-secret";
  await submit(get("connection-form"));
  await settle();
  const button = { dataset: { setupName: "model" }, disabled: false, textContent: "Test connection" };
  get("setup-list").listeners.click({ target: { closest: () => button } });
  await settle();
  const request = requests.find((item) => item.url === "/v1/admin/setup/connectors/model/test");
  assert.equal(request.options.method, "POST");
  assert.equal(request.options.headers.get("Authorization"), "Bearer operator-secret");
  assert.ok(textOf(get("setup-list")).includes("Verified."));
  assert.equal(get("setup-state").textContent, "Ready");
  assert.equal(button.disabled, false);
});

test("stale setup refresh cannot overwrite a newer probe result", async () => {
  let resolveSetup;
  const setupResponse = new Promise((resolve) => { resolveSetup = resolve; });
  const { get } = harness({
    "/v1/admin/overview": { body: { mode: "SHADOW", investigations: {}, approvals: {}, jobs: {} } },
    "/v1/admin/investigations?limit=100": { body: { investigations: [] } },
    "/v1/admin/approvals?status=PENDING": { body: { approvals: [] } },
    "/v1/admin/setup": () => setupResponse,
    "/v1/admin/setup/connectors/model/test": { body: {
      component: { name: "model", label: "Model", description: "Provider", permission: "Metadata", configured: true, probeable: true, state: "ready", message: "Verified." },
      setup: { state: "ready", features: {}, components: [{ name: "model", label: "Model", description: "Provider", permission: "Metadata", configured: true, probeable: true, state: "ready", message: "Verified." }] },
    } },
  });
  get("operator-token").value = "operator-secret";
  await submit(get("connection-form"));
  const button = { dataset: { setupName: "model" }, disabled: false, textContent: "Test connection" };
  get("setup-list").listeners.click({ target: { closest: () => button } });
  await settle();
  resolveSetup({ body: { state: "not_tested", features: {}, components: [] } });
  await settle();
  assert.equal(get("setup-state").textContent, "Ready");
  assert.ok(textOf(get("setup-list")).includes("Verified."));
});

test("setup serializes browser-initiated connector probes", async () => {
  let resolveProbe;
  const probeResponse = new Promise((resolve) => { resolveProbe = resolve; });
  const initial = { state: "not_tested", features: {}, components: [
    { name: "model", label: "Model", description: "Provider", permission: "Metadata", configured: true, probeable: true, state: "not_tested", message: "Not tested." },
    { name: "google", label: "Google", description: "Directory", permission: "Read", configured: true, probeable: true, state: "not_tested", message: "Not tested." },
  ] };
  const { get, requests } = harness({
    "/v1/admin/overview": { body: { mode: "SHADOW", investigations: {}, approvals: {}, jobs: {} } },
    "/v1/admin/investigations?limit=100": { body: { investigations: [] } },
    "/v1/admin/approvals?status=PENDING": { body: { approvals: [] } },
    "/v1/admin/setup": { body: initial },
    "/v1/admin/setup/connectors/model/test": () => probeResponse,
  });
  get("operator-token").value = "operator-secret";
  await submit(get("connection-form"));
  await settle();
  const modelButton = { dataset: { setupName: "model" }, disabled: false, textContent: "Test connection" };
  const googleButton = { dataset: { setupName: "google" }, disabled: false, textContent: "Test connection" };
  get("setup-list").listeners.click({ target: { closest: () => modelButton } });
  get("setup-list").listeners.click({ target: { closest: () => googleButton } });
  assert.equal(requests.filter((item) => item.url.includes("/test")).length, 1);
  resolveProbe({ body: { component: initial.components[0], setup: initial } });
  await settle();
});

test("operator review submits the edited literal text and original draft digest", async () => {
  const hostile = '<img src=x onerror="alert(1)">';
  const id = "review-case";
  const draft = "Original question";
  const messages = { messages: [{ id: "draft-1", direction: "OUTBOUND", source: "jira", body: draft }], review_enabled: true, draft_digests: { "draft-1": "original-digest" }, deliveries: [] };
  const { get, requests } = harness({
    [`/v1/investigations/${id}`]: { body: { status: "WAITING_ON_REQUESTER" } },
    [`/v1/investigations/${id}/messages`]: { body: messages },
    [`/v1/investigations/${id}/messages/draft-1/review`]: { status: 202, body: { created: true } },
  });
  openInvestigation(get, id);
  await settle();
  const form = descendants(get("dialog-body")).find((item) => item.className === "message-review");
  assert.ok(form);
  const fields = descendants(form);
  fields.find((item) => item.tagName === "textarea").value = `Edited question ${hostile}`;
  fields.find((item) => item.tagName === "input").value = " operator@example.test ";
  await submit(form);
  const request = requests.find((item) => item.url.endsWith("/review"));
  assert.deepEqual(JSON.parse(request.options.body), { body: `Edited question ${hostile}`, actor: "operator@example.test", draft_digest: "original-digest" });
  assert.equal(request.options.method, "POST");
  assert.ok(!descendants(get("dialog-body")).some((item) => item.tagName === "img"));
});

test("review conflict retains the draft and shows the server explanation", async () => {
  const { get } = harness({
    "/v1/investigations/conflict": { body: { status: "COMPLETED" } },
    "/v1/investigations/conflict/messages": { body: { messages: [{ id: "draft", direction: "OUTBOUND", body: "Result" }], review_enabled: true, draft_digests: { draft: "digest" } } },
    "/v1/investigations/conflict/messages/draft/review": { status: 409, body: { error: "Draft was superseded" } },
  });
  openInvestigation(get, "conflict");
  await settle();
  const form = descendants(get("dialog-body")).find((item) => item.className === "message-review");
  await submit(form);
  assert.ok(textOf(form).includes("Draft was superseded"));
  assert.equal(descendants(form).find((item) => item.tagName === "textarea").value, "Result");
  assert.equal(descendants(form).find((item) => item.tagName === "button").disabled, false);
});

test("review controls exclude shadow, superseded, and inbound messages", async () => {
  for (const [status, enabled, messages] of [
    ["COMPLETED", false, [{ id: "draft", direction: "OUTBOUND", body: "Shadow result" }]],
    ["PENDING", true, [{ id: "draft", direction: "OUTBOUND", body: "Obsolete question" }]],
    ["WAITING_ON_REQUESTER", true, [{ id: "draft", direction: "OUTBOUND", body: "Old question" }, { id: "reply", direction: "INBOUND", body: "Answer" }]],
  ]) {
    const { get } = harness({
      "/v1/investigations/guard": { body: { status } },
      "/v1/investigations/guard/messages": { body: { messages, review_enabled: enabled } },
    });
    openInvestigation(get, "guard");
    await settle();
    assert.ok(!descendants(get("dialog-body")).some((item) => item.className === "message-review"));
  }
});

test("reviewed delivery and provisional risk remain literal and cite their evidence", async () => {
  const hostile = '<script>alert(1)</script>';
  const { get } = harness({
    "/v1/investigations/delivered": { body: { status: "COMPLETED", work_package: { version: 1, summary: "Executive summary", findings: [], unknowns: [], risk_assessment: { rating: "MODERATE", rationale: hostile, evidence_ids: ["vendor-doc"] } } } },
    "/v1/investigations/delivered/messages": { body: { messages: [{ id: "draft", direction: "OUTBOUND", body: "Original" }], review_enabled: true, deliveries: [{ message_id: "draft", status: "SENT", reviewed_by: "operator", reviewed_body: hostile, remote_id: "comment-1" }] } },
  });
  openInvestigation(get, "delivered");
  await settle();
  const text = textOf(get("dialog-body"));
  for (const value of ["Provisional vendor risk", "MODERATE", "vendor-doc", "SENT", "operator", "comment-1", hostile]) assert.ok(text.includes(value));
  assert.ok(!descendants(get("dialog-body")).some((item) => item.tagName === "script" || item.className === "message-review"));
});

function recoveryHarness({ status = "NEEDS_RECONCILIATION", enabled = true, superseded = false, recover } = {}) {
  const delivery = { message_id: "draft", status, revision: 4, attempt: 1, reviewed_at: "2026-10-17T12:00:00Z", reviewed_by: "original-operator", reviewed_body: "Exact approved text" };
  const messages = [{ id: "draft", direction: "OUTBOUND", body: "Original draft" }];
  if (superseded) messages.push({ id: "reply", direction: "INBOUND", body: "New information" });
  const responses = {
    "/v1/investigations/recovery": { body: { status: "WAITING_ON_REQUESTER" } },
    "/v1/investigations/recovery/messages": () => ({ body: { messages, deliveries: [delivery], recovery_enabled: enabled } }),
    "/v1/investigations/recovery/messages/draft/recovery": recover || (() => { delivery.status = "SENT"; delivery.remote_id = "10001"; return { body: { outcome: "verified" } }; }),
  };
  const h = harness(responses);
  openInvestigation(h.get, "recovery");
  return { ...h, delivery };
}

function recoveryFields(get) {
  const form = descendants(get("dialog-body")).find((e) => e.className === "message-recovery");
  assert.ok(form, "Recovery controls missing");
  const field = (id) => descendants(form).find((e) => e.id === `recovery-${id}-draft`);
  const actor = field("actor"), reason = field("reason"), remote = field("comment"), ack = field("risk");
  actor.value = "operator@example.test";
  reason.value = "Checked Jira after restart";
  remote.value = "10001";
  return { form, actor, reason, remote, ack, resend: descendants(form).find((e) => e.textContent === "Resend approved message") };
}

test("delivery reconciliation sends the selected comment and revision without approving new text", async () => {
  const { get, requests } = recoveryHarness();
  await settle();
  const { form } = recoveryFields(get);
  await submit(form);
  const request = requests.find((r) => r.url.endsWith("/recovery") && r.options.method === "POST");
  assert.deepEqual(JSON.parse(request.options.body), { action: "reconcile", actor: "operator@example.test", reason: "Checked Jira after restart", revision: 4, remote_id: "10001" });
  const text = textOf(get("dialog-body"));
  assert.match(text, /Jira comment verified/);
  assert.match(text, /Exact approved text/);
  assert.ok(!descendants(get("dialog-body")).some((e) => e.className === "message-recovery"));
});

test("resend requires explicit duplicate-risk acknowledgement and submits once", async () => {
  let finish;
  const { get, requests } = recoveryHarness({ recover: () => new Promise((resolve) => { finish = resolve; }) });
  await settle();
  const { form, ack, resend } = recoveryFields(get);
  await resend.listeners.click();
  assert.match(textOf(form), /Acknowledge the risk/);
  assert.equal(requests.filter((r) => r.options.method === "POST").length, 0);
  ack.checked = true;
  const pending = resend.listeners.click();
  await settle();
  await resend.listeners.click();
  assert.equal(requests.filter((r) => r.options.method === "POST").length, 1);
  const request = requests.find((r) => r.options.method === "POST");
  assert.deepEqual(JSON.parse(request.options.body), { action: "resend", actor: "operator@example.test", reason: "Checked Jira after restart", revision: 4, acknowledge_duplicate_risk: true });
  finish({ body: { outcome: "resend_queued" } });
  await pending;
});

test("unsuccessful Jira verification preserves uncertainty and explains that nothing was sent", async () => {
  for (const outcome of ["comment_not_found", "comment_mismatch", "jira_verification_unavailable"]) {
    const { get } = recoveryHarness({ recover: () => ({ body: { outcome } }) });
    await settle();
    await submit(recoveryFields(get).form);
    const text = textOf(get("dialog-body"));
    assert.match(text, /Delivery remains unresolved; no message was sent/);
    assert.ok(descendants(get("dialog-body")).some((e) => e.className === "message-recovery"));
  }
});

test("recovery conflicts preserve operator input and render errors literally", async () => {
  const hostile = '<script>alert("conflict")</script>';
  const { get } = recoveryHarness({ recover: () => ({ status: 409, body: { error: hostile } }) });
  await settle();
  const { form, reason } = recoveryFields(get);
  await submit(form);
  assert.equal(reason.value, "Checked Jira after restart");
  assert.ok(textOf(form).includes(hostile));
  assert.ok(!descendants(form).some((e) => e.tagName === "script"));
});

test("recovery controls follow backend enablement and restrict superseded messages to verification", async () => {
  for (const status of ["PENDING", "RUNNING", "SENT", "SUPERSEDED"]) {
    const { get } = recoveryHarness({ status });
    await settle();
    assert.ok(!descendants(get("dialog-body")).some((e) => e.className === "message-recovery"));
  }
  const disabled = recoveryHarness({ enabled: false });
  await settle();
  assert.ok(!descendants(disabled.get("dialog-body")).some((e) => e.className === "message-recovery"));
  const historical = recoveryHarness({ superseded: true });
  await settle();
  const { form, resend } = recoveryFields(historical.get);
  assert.equal(resend, undefined);
  assert.match(textOf(form), /verify a past delivery, but cannot resend/);
});

test("a late recovery response cannot reopen or replace a different investigation", async () => {
  let finish;
  const { get } = recoveryHarness({ recover: () => new Promise((resolve) => { finish = resolve; }) });
  await settle();
  const pending = submit(recoveryFields(get).form);
  await settle();
  openInvestigation(get, "another-case");
  await settle();
  finish({ body: { outcome: "verified" } });
  await pending;
  assert.equal(get("dialog-title").textContent, "another-case");
  assert.ok(!textOf(get("dialog-body")).includes("Jira comment verified"));
});

test("Go vendor scoring exposes rubric, context, criterion interpretations and evidence gaps", async () => {
  const hostile = '<img src=x onerror="alert(1)">';
  const report = { version: 1, summary: "Vendor evidence review", findings: [{ kind: "vendor_claim", statement: hostile, evidence_ids: ["doc"] }], unknowns: [], risk_assessment: {
    rating: "UNKNOWN", rationale: "Required evidence is incomplete", evidence_ids: ["doc"], scoring: {
      method: "weighted-v1", rubric_digest: "stored-rubric-digest", context: { vendor: "acme", deployment: { use_case: "Internal collaboration", data_classification: "Internal", access_scope: "Read only" }, rubric: { version: "fixture-v1", moderate_at: 25, high_at: 60, criteria: [{ id: "assurance", weight: 100, evidence: "independent_report", requirement: "Independent report covers the service" }] } },
      assessment: { criteria: [{ id: "assurance", status: "unknown", reason: hostile, evidence_ids: ["doc"] }] },
      gaps: [{ code: "independent_report_missing", detail: "A public vendor claim does not establish independent assurance", evidence_ids: ["doc"] }],
    },
  } };
  const { get } = harness({
    "/v1/investigations/vendor-score": { body: { work_package: report } },
    "/v1/investigations/vendor-score/timeline": { body: { events: [{ type: "evidence.collected", actor_type: "CONNECTOR", actor_id: "research", data: { evidence_id: "doc", claim_type: "vendor_claim" } }] } },
  });
  openInvestigation(get, "vendor-score");
  await settle();
  const text = textOf(get("dialog-body"));
  for (const expected of ["Go · weighted-v1", "stored-rubric-digest", "fixture-v1", "Internal collaboration", "Read only", "Withheld", "independent_report", "Criterion assessments", "Evidence gaps", "Vendor claims", hostile]) assert.ok(text.includes(expected), `Missing ${expected}`);
  assert.ok(!descendants(get("dialog-body")).some((e) => e.tagName === "img"));
});

test("a complete zero-point Go assessment shows zero and preserves its policy thresholds", async () => {
  const { get } = harness({ "/v1/investigations/vendor-zero": { body: { work_package: { version: 1, summary: "Review", risk_assessment: { rating: "LOW", scoring: { method: "weighted-v1", score: 0, context: { rubric: { version: "fixture-v1", moderate_at: 25, high_at: 60 } }, assessment: { criteria: [] }, gaps: [] } } } } } });
  openInvestigation(get, "vendor-zero");
  await settle();
  const text = textOf(get("dialog-body"));
  assert.ok(text.includes("0/100"));
  assert.ok(text.includes("MODERATE at 25, HIGH at 60"));
  assert.ok(text.includes("No rubric gaps recorded"));
});

test("historical model-assigned vendor ratings remain readable and explicitly labelled", async () => {
  const { get } = harness({ "/v1/investigations/vendor-legacy": { body: { work_package: { version: 1, summary: "Historical review", risk_assessment: { rating: "MODERATE", rationale: "Legacy interpretation", evidence_ids: [] } } } } });
  openInvestigation(get, "vendor-legacy");
  await settle();
  assert.match(textOf(get("dialog-body")), /Legacy model-assigned rating/);
});
