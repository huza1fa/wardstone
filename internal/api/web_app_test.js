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
  assert.ok(textOf(get("dialog-body")).includes("does not confirm delivery"));
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
