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
