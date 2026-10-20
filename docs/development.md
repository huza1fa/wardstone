# Development

## Requirements

- Go 1.25 or later
- Docker with Compose

## Local database

```sh
docker compose up -d postgres
```

The Compose service applies SQL files in `migrations/` when creating a fresh
volume. To apply a new migration to an existing development volume, run it
explicitly, for example:

```sh
docker compose exec -T postgres psql -U wardstone_admin -d wardstone \
  < migrations/000010_delivery_recovery.sql
```

Existing schema-10 installations also need migrations 11 and 12; see the
[upgrade notes](release-2026-10-19.md).

Never reapply a migration whose version already appears in
`schema_migrations`.

The current runtime requires schema version 12. Apply any missing migrations in
numeric order. Migration 7 preserves old cases without inventing provenance;
new dispatches record their original routing decision. Migration 8 adds evidence
collector capability provenance, a newest-first case/capability index, and an
immutable completion-time `work_package`. Existing evidence keeps an empty
capability and is hidden from model case memory; existing investigations may
have no report. Migration 9 adds immutable exact-text message reviews; legacy
delivery rows retain no review and are excluded from the REVIEW dispatcher. Migration
10 distinguishes failed, uncertain, and superseded deliveries and adds recovery
revisions and comment-correlation requirements. Old `DEAD` rows become uncertain
unless their recorded error identifies a superseded draft. Original errors and
reviews remain intact.

## Google identity test tenant

For the bounded user/group membership pilot, start with **Cloud Identity Free**.
Google lists directory management, organizational groups, and API automation in
the free edition, with 50 user licenses by default. This is a real managed
identity tenant, without Workspace Gmail hosting; it is useful for our identity
tests rather than a full Workspace application test environment. See Google's
[edition comparison](https://docs.cloud.google.com/identity/docs/editions),
[pricing](https://cloud.google.com/identity/pricing), and
[Free signup instructions](https://docs.cloud.google.com/identity/docs/how-to/set-up-cloud-identity-admin).

Use a dedicated domain you control and verify it through DNS. Domain registration
is a separate cost if you do not already own a suitable domain. Google also notes
that administrator notifications require external email hosting or forwarding.
Create one administrator, two synthetic users, and a test group. Enable the Admin
SDK API in a dedicated Google Cloud project, obtain an appropriately scoped admin
OAuth token, and first verify Wardstone's user, group, and direct-membership reads.
The existing connector uses the Admin SDK Directory API; confirm its scopes and
admin permissions against the tenant before proceeding to a write pilot.

The current runtime still denies Google mutations in SHADOW and REVIEW. Only
after the approved executor is implemented should the pilot add a synthetic user
to the test group and independently read membership to verify the result. The
Directory API's [membership insertion reference](https://developers.google.com/workspace/admin/directory/reference/rest/v1/members/insert)
documents the write operation and required scopes. Tenant provisioning and live
API compatibility have not yet been verified for this project.

If full Workspace apps are needed later, Google offers a
[14-day Workspace trial](https://support.google.com/a/answer/6388094).
That trial requires payment setup and can begin charging after the trial;
Cloud Identity Free is the better starting point for an ongoing identity lab.

## Specialist profiles

`WARDSTONE_CONFIG_FILE` is administrator-owned configuration. It defines both
capability policy and specialist profiles. A profile has bounded instructions,
an allowlist of read collectors, an allowlist of capabilities it may propose,
and explicit handoff targets. Follow the supplied configuration when adding a
specialist. A profile never grants execution authority—every proposal still passes deterministic
capability validation and policy.

The example policy includes Help Desk, Access Management, Vendor Review, and
Systems Knowledge profiles. Vendor Review allows `research.vendor.collect` and
Systems Knowledge allows `research.knowledge.collect`. Only configured sources
are readable; defining a profile alone does not create document access.

## Checks

```sh
go test ./...
go test -race ./...
go vet ./...
node --test internal/api/web_app_test.js
```

The frontend tests use Node.js 22 or later. Set `WARDSTONE_TEST_DATABASE_URL`
to a migrated PostgreSQL test database to run persistence and API/worker
integration tests. The end-to-end routing test creates and removes its own
schema so it cannot claim unrelated jobs.

The Go process intentionally does not load `.env` files. Export the values from
`.env.example` with your preferred local environment tool before running
`go run ./cmd/wardstone`. Secrets must not be committed. Set
`WARDSTONE_CONFIG_FILE` to the capability policy file; the example environment
uses `examples/wardstone.yaml`.

`WARDSTONE_DATABASE_URL`, `WARDSTONE_OPERATOR_TOKEN`, and
`WARDSTONE_CONFIG_FILE` are the bootstrap requirements. Jira intake, Jira API,
Google Workspace, and the model provider may be configured independently. A
partially configured Jira API group is rejected: base URL, account email, and
API token must be supplied together. Workers do not start until a model is
configured, so pending jobs are not consumed by an incomplete installation.

## Admin web console

Start Wardstone, then open `http://127.0.0.1:8080/admin/` (or the configured
listen address). Enter `WARDSTONE_OPERATOR_TOKEN` when prompted. The token is
kept in browser session storage for the current tab and removed when you
disconnect.

The console shows the operating mode, investigation and job status, recent
diagnoses and timelines, and pending approval requests. Investigation detail
also shows the structured report, verified findings separately from assumptions
and unknowns, literal evidence IDs with available timeline provenance and
observation times, exact proposed argument JSON, policy outcomes, and requester
conversation/drafts. Observation age is calculated at display time; it does not
prove that a citation was collected in the current run. Missing legacy reports
and missing provenance are shown explicitly. Browser and terminal
approval decisions both use the same authenticated API and approval service.
SHADOW and REVIEW both deny infrastructure mutations, so the infrastructure
approval queue remains empty. REVIEW adds message review controls to case detail.

The Setup section reports which runtime features are enabled and which
credentials are present without returning secret values. Live connection tests
are operator-authenticated, manually initiated, read only, limited to one
in-flight test per connector, and bounded to five seconds. Jira tests the
authenticated identity endpoint, Google requests one basic directory user,
and the model provider reads metadata for the configured model. Provider
redirects are not followed. Jira Automation webhook delivery cannot be tested
from Wardstone and is reported as locally configured only.

The corresponding authenticated endpoints are `GET /v1/admin/setup` and
`POST /v1/admin/setup/connectors/{name}/test`. `/healthz` remains a public
liveness endpoint and intentionally exposes no connector information.

## OpenRouter pilot configuration

Wardstone does not automatically load `.env`; export selected variables before
starting the process. `WARDSTONE_AI_KEY` is an OpenRouter shortcut. When no
explicit model endpoint/key is set, it selects `https://openrouter.ai/api/v1`
and defaults to `openai/gpt-4.1-mini`. Set `WARDSTONE_OPENAI_MODEL` to choose a
different compatible model. An explicit `WARDSTONE_OPENAI_API_KEY` takes
precedence. A shortcut key with a conflicting endpoint fails configuration,
rather than forwarding that key to another provider.

OpenRouter automatically uses strict JSON Schema response variants. For another
endpoint that supports them, set `WARDSTONE_MODEL_STRUCTURED_OUTPUT=true`.
Other endpoints keep the existing JSON-object protocol. Both paths retain the
same runtime validation and bounded token budgets. OpenRouter readiness uses
the model's endpoints metadata API.

## Reviewed requester messages

Set `WARDSTONE_MODE=REVIEW` and configure the Jira API account's Add comments
permission. Investigations still create drafts. Open case detail in `/admin/`,
edit **Exact message to send**, supply operator identity, and choose **Approve
and send to Jira**. The approved text is immutable and visible alongside the
original draft, delivery state, and remote comment ID. REVIEW does not enable
Google mutations or infrastructure-action approvals.

The authenticated API is:

```text
GET /v1/investigations/{id}/messages
POST /v1/investigations/{id}/messages/{message_id}/review
{"body":"exact approved message","actor":"operator@example.com","draft_digest":"SHA-256 from the GET response"}
```

The GET response includes `draft_digests`, `deliveries`, and `review_enabled`.
Review requires the original draft's digest, even when its text is edited.
Identical retries return the same delivery; different reviews conflict. Only
the latest outbound draft in a waiting or completed case can receive a new
review. A replied-to question is superseded. SHADOW never accepts sends.

Delivery failures are explicit. A `delivery_outcome_unknown` or ambiguous Jira
POST result requires checking Jira before any manual resend. There is no
automatic reconciliation/resend control in this pilot. Rate-limit responses
receive bounded retries; uncertain remote outcomes do not.

## Vendor sources and systems runbooks

Configure `research.vendors` and `research.knowledge` in the policy YAML. Each
resource contains one through four titled sources. Both catalogs accept public
HTTPS text documents and configured `.pdf`, `.md`, or `.txt` files. Relative
paths resolve from the policy file. The example config includes Tailscale public
sources; local evidence stays outside Git and is selected only by administrator
configuration. No browser upload or Google Workspace setup is required.

```yaml
research:
  vendors:
    Acme:
      - title: Acme security questionnaire
        path: ../evidence/acme/questionnaire.md
      - title: Acme independent assurance report
        path: ../evidence/acme/audit.pdf
        claim_type: independent_report
  knowledge:
    VPN troubleshooting:
      - title: Corporate VPN troubleshooting
        path: ../runbooks/vpn.md
```

Use exact Jira fields `Vendor: ["Tailscale"]` or
`Knowledge topic: ["VPN troubleshooting"]`. Missing fields can be supplied by
a whole requester reply matching a configured name. Explicit invalid values
need an operator-corrected new case; replies cannot override them. Public URL
targets come exclusively from the catalog. Source failures become report gaps.

Examples of supported Jira intake payloads:

```json
{"external_id":"PILOT-VENDOR-1","summary":"Do a vendor review on Tailscale and give an executive summary plus a risk rating","request_type":"Vendor review","reporter_email":"requester@example.com","fields":{"Vendor":["Tailscale"]}}
{"external_id":"PILOT-KNOWLEDGE-1","summary":"How do I troubleshoot VPN authentication?","request_type":"Systems question","reporter_email":"requester@example.com","fields":{"Knowledge topic":["VPN troubleshooting"]}}
```

The example routes these structured request types directly. Prose-only requests
use the intent fallback and may need a selector clarification. Vendor reports
include `risk_assessment.rating` (`LOW`, `MODERATE`, `HIGH`, `UNKNOWN`), rationale,
and citations. Go now computes the rating from the configured rubric and cited
criterion assessments. Missing rubric, deployment context, or required evidence
produces UNKNOWN with no numeric score. See [vendor risk scoring](#vendor-risk-scoring).
Public vendor statements remain claims; private reports are not inferred.

Text documents are limited to 512 KiB reads and 24 KiB excerpts. Local PDFs
are limited to 8 MiB and 20 pages, with 24 KiB of retained text **across the whole
file**. Each readable PDF page gets a separate evidence ID, page number/count,
original-file SHA-256, extraction method, and observation time. The audit console
shows this provenance without exposing filesystem paths or document bodies.
Administrator classification (`vendor_claim` by default, or `independent_report`)
is retained on vendor pages; a private file does not automatically become
independent assurance. Knowledge files are labelled `administrator_document`.

PDF extraction requires Linux, Poppler (`pdfinfo`, `pdftotext`), Bubblewrap, and
`prlimit`. On Debian/Ubuntu install them with:

```sh
sudo apt-get install poppler-utils bubblewrap util-linux
```

The service account must be able to use Bubblewrap's unprivileged user/network/PID
namespaces. Extraction has a five-second deadline across both parser commands,
a 512 MiB address-space limit, three CPU seconds per command, bounded output
(256 KiB stdout, 16 KiB diagnostics), and no inherited server environment.
Only system executables/libraries/fonts are mounted read only; the parser receives
the original byte snapshot on stdin and has no source-file/home/network access.
If tools or sandbox permissions are missing, collection records a gap with no
unsandboxed fallback. Existing public text sources continue to work.

Ubuntu 24.04 also requires an AppArmor profile granting `userns,` to Bubblewrap.
Follow Ubuntu's [application profile guidance](https://discourse.ubuntu.com/t/ubuntu-24-04-lts-noble-numbat-release-notes/39890#unprivileged-user-namespace-restrictions-14)
when configuring a host where unprivileged user namespaces are restricted.
Installing the parser packages alone may still leave extraction unavailable.
CI pins Ubuntu 24.04, loads a profile attached to `/usr/bin/bwrap` on its disposable
runner, and checks namespace startup before running the mandatory extraction tests.
The workflow does not change the system-wide namespace restriction.

Keep configured file directories under administrator control. Each configured
file opens relative to a confined directory handle; escaping symlinks and
nonregular files are rejected. Linux is required for confined local file reads.
Neither tickets nor models can supply paths. Do not place private evidence in Git.
Retained evidence excerpts are stored in PostgreSQL and sent to the configured
model provider during investigation, so use only documents that provider is
permitted to process.

Encrypted/malformed/over-limit PDFs, parser warnings, missing text layers, and
omitted pages remain explicit `research.document_unavailable` observations.
Affected page numbers are recorded for empty-text and omitted pages. A blank
page is conservatively treated as missing text too. Readable pages can still be
cited, but any gap or truncated excerpt withholds the vendor score. There is no
OCR, arbitrary search, authenticated portal access, or remote PDF download in
this slice. Public HTTP sources remain text/HTML/Markdown only.

Synthetic PDF fixtures live in `internal/connectors/research/testdata`; the
optional `generate.py` reproduces them with ReportLab and pypdf. The normal test
suite reads the checked-in PDFs using the production sandbox. CI installs the
parser dependencies and fails if isolation or real extraction does not work.

## Jira API account

Create a dedicated Jira Cloud service account with only **Browse projects** for
the development project. Create an Atlassian API token for
that account and configure the ignored local environment file:

```sh
WARDSTONE_JIRA_BASE_URL=https://your-site.atlassian.net
WARDSTONE_JIRA_EMAIL=wardstone-bot@example.com
WARDSTONE_JIRA_API_TOKEN=replace-me
WARDSTONE_JIRA_WEBHOOK_SECRET=replace-with-a-separate-random-secret
```

The service uses Jira's REST v3 comment endpoint. It requires HTTPS in normal
operation; `http://localhost` and loopback IPs are accepted only for test mocks.
Do not reuse the outbound Jira API token as the inbound webhook secret.

SHADOW mode stores model-generated questions and results as operator-visible
drafts only. It does not post requester comments or require **Add comments**
permission. Explicit connector-readiness probes can still read Jira metadata.
The transactional `message_deliveries` outbox, leased dispatcher, and Jira
comment sender already exist. SHADOW disables delivery enqueueing and dispatcher
startup. Set WARDSTONE_MODE=REVIEW to enable exact operator-reviewed sending.
The dedicated Jira account then needs Add comments permission.

## Bounded access help

Configure an administrator-owned resource-to-Google-group map in
`WARDSTONE_CONFIG_FILE`:

```yaml
access_help:
  resources:
    Engineering tools: engineering-tools@example.com
    Finance reports: finance-reports@example.com
```

Resource names are trimmed in configuration, matched case-insensitively, and
must be unique after normalization. The map accepts at most 128 entries, each
name at most 128 bytes with no control characters, and plain group email
addresses. An omitted or empty map is valid and leaves the access collector
unregistered. With Google configured and a nonempty map, profiles must allow
the read collector `google.access.inspect` to receive its observations. The
example grants this collector to Access Management; Help Desk hands confirmed
access requests to that specialist. Proposal
permissions and capability policy remain separate.

Send the exact structured field keys in the flattened Jira `fields` map, each
with exactly one value:

```json
{
  "external_id": "HELP-42",
  "summary": "Help with Engineering tools access",
  "reporter_email": "requester@example.com",
  "request_type": "Access request",
  "fields": {
    "Access resource": ["Engineering tools"],
    "Access user": ["requester@example.com"]
  }
}
```

- **Access resource** must match one configured name in its entirety, ignoring
  letter case. A supplied field is not whitespace-trimmed. Unknown resources,
  blank/multiple values, duplicate near-matching keys, or non-exact field keys
  require operator intervention. The collector does not infer a resource from ticket
  prose, keywords, group addresses, or URLs.
- Only when **Access resource is absent** may the latest durable inbound reply
  from the ticket's own requester and source supply it. After trimming outer
  whitespace, the **whole reply** must be the configured resource name, such as
  `Engineering tools`. A sentence like `Please grant Engineering tools access`
  does not match. An explicitly supplied invalid resource field cannot be
  overridden by a reply. Reply intake authenticates the webhook, checks the
  reporter identity, and deduplicates stable comment IDs.
- **Access user** must be one plain email address. If absent, the ticket reporter
  is used. An explicitly supplied invalid field cannot silently default to the
  reporter; requester free text cannot select another target user.

Only an absent resource with a valid user is reply-resolvable. Invalid explicit
selectors or a missing/malformed target user produce
`operator_intervention_required` with `escalation_requirements`, without external
reads. An operator must correct the selectors and submit a new external case ID:
re-intake deduplicates an existing case and does not refresh its original fields.
Do not repeatedly ask the requester to repair immutable case metadata.

For a missing resource, the collector emits a structured `google.access_request`
observation with unknowns and clarification requirements without performing the
directory reads. Otherwise it reads the selected user
and mapped group, verifies returned identities, then checks that user's direct
`USER` membership using the returned IDs. Results distinguish `member`,
`not_member`, and `unknown`; unavailable, forbidden, or malformed responses
remain unknowns. A not-found membership response means no direct membership
only after both user and group were resolved successfully.

When the access collector is installed, deterministic proposal validation applies
even to specialists that cannot run it. A group addition must cite a complete,
fresh current-run inspection of the exact configured group and nonsuspended
target user with no unknowns, clarifications, or escalations and missing direct
membership. Contradictory or mismatched proposals fail before reports, actions,
or requester drafts are published. Group removals are outside this bounded
workflow and are rejected; the example disables them.

**Scope:** the map is administrator context, not authorization to grant access.
Direct Google group membership does not prove effective application access,
nested/transitive membership, licensing, provisioning, or business approval.
Read observations and proposed changes are review material. SHADOW sends no
requester messages and performs no Google writes, even when configuration lists
a mutation capability in approval mode.

See [case memory and reports](architecture.md#bounded-case-memory-and-reports)
for input budgets, provenance filtering, report schema, and mutation-citation
freshness rules.

## Jira ticket routing metadata

Jira Automation should send a small, flattened routing subset with each ticket.
Wardstone evaluates configured `routing.rules` against `issue_type`,
`request_type`, `components`, `labels`, and selected `fields` before it makes
an intent-classification model call. The fields map is intentionally an
allowlisted automation output, not a raw Jira issue payload.

For Jira cascading selects, flatten the selected parent and child into stable
keys such as `customfield_12345.parent` and `customfield_12345.child`. A rule can
require both keys; Wardstone does not interpret arbitrary dotted JSON paths.

Use `match.sources: [jira]` to restrict a rule to Jira tickets. Without a source
selector, the rule can match the metadata supplied by any connector.

```json
{
  "external_id": "HELP-42",
  "summary": "Review Acme's security posture",
  "description": "Procurement needs a recommendation.",
  "reporter_email": "operator@example.com",
  "issue_type": "Service Request",
  "labels": ["vendor"],
  "fields": {"Review type": ["Security"]}
}
```

Rules are ordered: the first match wins. Values within one selector are
alternatives; every selector in a rule is required. Values and source names
ignore surrounding whitespace and letter case. Custom field **keys** ignore
surrounding whitespace but preserve case, so `Review type` and `review type`
identify different fields. Duplicate trimmed field keys and rule names, empty
selectors, unknown YAML options, and oversized rule sets are rejected during
startup. The limits are 128 rules, 64 custom fields per rule, 32 alternatives
per selector, 4,096 selectors in total, and 16,384 alternatives in total.
Ticket summaries and descriptions never trigger hidden keyword routes.

If no structured rule matches, `routing.intent` controls the optional model:

```yaml
routing:
  intent:
    enabled: true
    min_confidence: 0.75
    timeout: 10s
```

These are the defaults when the intent settings are omitted. Set `enabled:
false` to send unmatched tickets directly to Help Desk. Confidence must be
greater than zero and at most one; timeout must be positive and at most five
minutes. The classifier may select only installed specialist profiles. An
unavailable, timed-out, invalid, or low-confidence classification safely stays
with `help_desk`, where the durable follow-up workflow can collect the missing
fact. Routing decisions record whether a rule, model, or fallback selected the
role, together with the rule name or fallback reason and model confidence when
available. A routing decision grants no new permissions: Help Desk and every
specialist retain their configured collectors, proposals, and handoff targets.

Intent classification receives bounded ticket text and selected routing
metadata, omitting the requester identity and case/ticket identifiers. The
encoded request budget is 64 KiB; descriptions are truncated safely at 16 KiB.
An input exceeding that budget records `intent_input_too_large` and assigns
Help Desk. Model redirects are rejected to prevent forwarding ticket data to
another endpoint. Confidence is the model's estimate, not a calibrated
probability or an authorization decision.

The initial decision and `dispatcher.routed` audit event are persisted together
once. Resumes and handoffs do not rerun classification or rewrite its history.
The web and terminal consoles show both initial routing and the current
specialist, including fallback codes, confidence, and classification model.

## Routing preview

Use the web console's Routing preview form or authenticated
`POST /v1/admin/routing/preview` with the same Jira ticket payload. The response
is `{ "routing": { ... } }`. Preview evaluates rules only, stores nothing, and
does not call a model. If no rule matches, it explains whether live intake
would attempt intent classification or assign Help Desk directly.

## Requester replies

When an investigation needs one missing fact, the model stores a durable
outbound question and the investigation moves to `WAITING_ON_REQUESTER`. The
current Jira Automation-friendly reply endpoint is:

```text
POST /v1/tickets/jira/replies
Authorization: Bearer $WARDSTONE_JIRA_WEBHOOK_SECRET
Content-Type: application/json

{"external_id":"HELP-42","comment_id":"jira-comment-id","author":"requester@example.com","body":"The asset tag is LT-1042"}
```

`comment_id` must be stable: repeating the same reply is safe and returns the
existing investigation without creating another resume job. Operators can read
the immutable conversation through
`GET /v1/investigations/{id}/messages` with `WARDSTONE_OPERATOR_TOKEN`.

The `author` must exactly match the ticket `reporter_email`; other commenters,
including the Wardstone bot account, receive `403 Forbidden`. Configure the
Automation rule to exclude the Wardstone service account as a second layer of
protection.

For Jira Automation, map the issue key to `external_id`, the comment's stable
ID to `comment_id`, the author identity to `author`, and the comment text to
`body`. The Automation web request must include:

```text
Authorization: Bearer $WARDSTONE_JIRA_WEBHOOK_SECRET
Content-Type: application/json
```

## Terminal console

Run the Charm-based terminal interface against the same Wardstone process:

```sh
WARDSTONE_OPERATOR_TOKEN=change-me-too go run ./cmd/wardstone-tui
```

`WARDSTONE_API_URL` defaults to `http://127.0.0.1:8080`.
`WARDSTONE_OPERATOR_ACTOR` sets the identity written to approval audit events;
it defaults to the current operating-system user. The equivalent command-line
flags are `-url`, `-actor`, and `-refresh`.

Use `1`/`2`/`3` or `tab` to switch views, arrows or `j`/`k` to move, `r` to
refresh, and `q` to quit. On the approvals view, `a` grants and `d` denies; both
require a final `y` confirmation.

The terminal console polls compact admin investigation summaries; it does not
load full investigation reports or conversation ledgers. Use the web detail
view or authenticated detail API to review the work package and exact proposals.


## Jira delivery recovery

With Jira configured in REVIEW mode, open a case's **Requester conversation**.
Stored conversations and recovery remain available without a configured model;
new investigations and requester-reply processing still follow model availability.
`NEEDS_RECONCILIATION` means a send may have reached Jira. `FAILED` means the
connector reported a definite rejection. `SUPERSEDED` means the dispatcher
suppressed a queued message after a newer message or case state replaced it.
Use **Refresh delivery status** to reload the current state and attempt count.

To reconcile an uncertain or failed delivery, inspect the original Jira issue,
enter the matching comment's ID, your operator identity, and a reason, then
choose **Verify Jira comment**. Wardstone reads its Jira account identity and
that one issue-scoped comment. It requires the bot author, exact approved ADF
body, and a creation time at or after the delivery's creation. New sends also
carry a `wardstone.message` property containing the message ID and approved-text
SHA-256; verification requires that property when the dispatcher recorded that
it would be sent. Legacy sends can be checked without the property. A mismatched
property is always rejected. A comment already reconciled to another message in
the same investigation cannot be reused.

Only a positive verification records `SENT` and a remote comment ID. A missing
comment, forbidden read, timeout, malformed response, changed body, or mismatch
leaves delivery unresolved. Reads are bounded to five seconds and one MiB per
response, reject redirects, and never return upstream bodies. Verification is
strict: Jira-side formatting edits or a changed bot account can prevent a match.
Verification records the confirmation time as `delivered_at`; it does not infer
an exact historical send time. The Jira API operations are documented in the
[Atlassian comment reference](https://developer.atlassian.com/cloud/jira/platform/rest/v3/api-group-issue-comments/).

To resend, supply your identity and reason, acknowledge that a duplicate comment
may result, then choose **Resend approved message**. This queues the original
approved text without editing or reapproving it. Only the latest outbound message
in a waiting or completed case can be requeued; the dispatcher checks again
before sending. A reply received after requeue suppresses that resend. Past
uncertain deliveries can still be positively verified after the conversation
moves on. Requester-message recovery never authorizes infrastructure changes.

The authenticated API is:

```text
POST /v1/investigations/{id}/messages/{messageID}/recovery
Authorization: Bearer $WARDSTONE_OPERATOR_TOKEN
Content-Type: application/json
```

Use the current delivery `revision` from `GET /v1/investigations/{id}/messages`:

```json
{"action":"reconcile","revision":2,"actor":"operator@example.com","reason":"Checked the issue after a service restart","remote_id":"10001"}
```

Or deliberately requeue the exact approved text:

```json
{"action":"resend","revision":2,"actor":"operator@example.com","reason":"Checked Jira; accept possible duplicate delivery","acknowledge_duplicate_risk":true}
```

A successful request returns `{ "delivery": { ... }, "outcome": "..." }`.
Outcomes are `verified`, `resend_queued`, `comment_not_found`, `comment_mismatch`,
or `jira_verification_unavailable`. Every recorded check advances the revision,
including checks that leave the send unresolved. A stale or ineligible request
returns HTTP 409; refresh before deciding again. A lost response can be resolved
by reloading delivery state and its audit timeline; retrying an old revision
cannot enqueue another send. SHADOW, unavailable Jira delivery, and absent
recovery configuration return HTTP 503. The messages read exposes
`recovery_enabled` for operator surfaces.

Claimed send attempts, retry scheduling, failures, confirmation, and operator
recovery decisions commit with append-only audit events. The original draft and
review are immutable. Authentication remains the single-administrator bearer
token; actor names are operator assertions, not independently authenticated user
identities. This recovery workflow is tested with simulated Jira endpoints and
PostgreSQL; a real Jira tenant still needs pilot verification. Ambiguous POSTs
remain capable of producing duplicates after an explicit resend.


## Vendor risk scoring

Go owns the overall rating for new vendor reports. The model returns
`vendor_assessment.criteria` with a configured criterion ID, `met`, `partial`,
`unmet`, or `unknown`, a concise reason, and supplied vendor-document evidence IDs.
It cannot return `risk_assessment` or an overall score. Questions/handoffs and
other specialists cannot attach vendor assessments. Missing criterion assessments
are retained as unknowns rather than silently treated as satisfied.

Declare an administrator-owned rubric and deployment context in the policy file:

```yaml
vendor_risk:
  rubric:
    version: example-v1
    moderate_at: 25
    high_at: 60
    criteria:
      - id: identity
        requirement: Identity controls match the declared deployment scope.
        weight: 40
        evidence: vendor_claim
      - id: assurance
        requirement: Independent assurance covers the intended service.
        weight: 60
        evidence: independent_report
  deployments:
    Example vendor:
      use_case: Internal collaboration
      data_classification: Internal
      access_scope: Read-only access to selected documents
```

This is example policy, not an industry-calibrated standard. Review the criteria,
thresholds, source designations, and intended deployment before using a rating.
The repository example includes four 25-point criteria and deliberately leaves
`deployments` empty so it cannot invent the intended Tailscale deployment.
Criteria IDs are unique lowercase identifiers, at most 48 characters. Configure
1–8 criteria, positive integer weights summing to 100, and thresholds with
`1 <= moderate_at < high_at <= 100`. Context strings and requirements are each
bounded to 600 bytes; at most 128 vendor deployments are permitted.

Deployment keys normalize whitespace and case and match the research catalog
selector. `use_case`, `data_classification`, and `access_scope` all need actual
administrator declarations. Ticket fields, model assumptions, and collected
website text cannot supply or override scoring policy/context. The current-run
research selector chooses the vendor; a historical selector cannot select scope.

When the review is complete, `met` contributes zero risk points, `partial` half
its criterion weight, and `unmet` the full weight. Sum contributions and round up
once at the end. A score below `moderate_at` is LOW; one at or above it is
MODERATE unless it meets `high_at`, which is HIGH. **Any evidence gap instead
produces UNKNOWN and withholds the numeric score**, including:

- Missing rubric, unresolved vendor, or missing administrator deployment context.
- Missing/unknown criterion assessments or conflicting evidence marked unknown.
- Missing current documents, omitted model-context sources, unavailable configured
  sources, or truncated excerpts.
- Historical citations or a criterion requiring an independent report when only
  vendor claims were cited.

Criterion citations must reference matching-vendor documents actually supplied
to the run with explicit research capability provenance. Only current-run sources
can complete a score. No documents means UNKNOWN. Unknowns do not count as zero
risk. A partial or unavailable review may still identify concerns in its findings;
UNKNOWN is not an approval or a statement that no problems were found.

Catalog sources default to `claim_type: vendor_claim`. An administrator may label
a reviewed source `claim_type: independent_report` in `research.vendors`:

```yaml
research:
  vendors:
    Example vendor:
      - title: Administrator-reviewed independent assurance report
        url: https://auditor.example/report.txt
        claim_type: independent_report
```

This label records administrator judgment about the source; the collector does
not independently authenticate an auditor or certify controls. A vendor web page
claiming an audit is not itself the independent report. Knowledge/runbook sources
cannot use this designation. Existing public text/HTML/Markdown collection limits
still apply; authenticated portals and PDF parsing remain outside this slice.

The immutable report's `risk_assessment.scoring` stores `weighted-v1`, the rubric
snapshot and SHA-256, deployment context, criterion assessments, evidence gaps,
and a score only when complete. `vendor.risk_scored` is a SYSTEM audit event
committed with report completion. Historical model-assigned ratings remain
readable and are labelled as legacy; changing the rubric never rewrites them.
Vendor scoring itself needs no additional schema migration. The current runtime
requires schema 12 for operator cases and the synthetic execution pilot; see the
[upgrade notes](release-2026-10-19.md).

The console distinguishes model-interpreted criteria, vendor claims, and gaps.
The runtime labels findings citing vendor claims accordingly. Bounded wording
checks reject common SOC 2 certification phrases and model-authored risk labels;
these checks are not a general semantic fact checker. Operators must still
review the substance of every assessment and citation. Go generates the vendor
requester-summary draft from its computed rating and gaps, preserving normal
exact-message approval and delivery. The model's requester summary cannot assign
an alternative final rating.

`internal/domain/testdata/vendor_risk_evaluations.json` contains 12 synthetic
scoring scenarios with expected outcomes. Domain tests add malformed/forged
assessments, strict snapshot decoding, cloning, rounding, and wording checks;
provider/API tests exercise schema authority and persistence. These are a seed
evaluation set for review, not evidence of expert calibration or live-model
semantic accuracy. They require no Workspace tenant or external model account.

## Operator cases, synthetic execution, and evaluations

See the [operator guide](operator-guide.md) for console workflows, API semantics,
synthetic execution phases, recovery, and model evaluation commands. The
[October 19 upgrade notes](release-2026-10-19.md) cover migrations 11 and 12 and
operating the service while the primary administrator is away.
