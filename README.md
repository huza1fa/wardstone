# Wardstone

Wardstone is a self-hosted virtual IT team for solo administrators and small
teams. It takes the interrupt-driven L1/L2 work that fragments an operator's
day, gathers the missing context, and returns a decision-ready result instead
of another chat thread to manage.

Its canonical [operating model](docs/operating-model.md) is a lightweight
dispatcher coordinating least-privilege operational specialists. Models may
investigate and propose; deterministic policy and explicit authority controls
all meaningful state changes.

Wardstone is not a chatbot, ITSM, MDM, or hosted service. Administrators retain
their infrastructure, credentials, policy, and model-provider accounts.

## Project intent

The goal is to reduce context switching without giving an AI model unchecked
access to infrastructure. Specialized investigation roles may research,
triage, clarify vague requests, and propose work; deterministic code owns
policy, approval, execution, and auditing.

Wardstone is intended to:

- ingest work from existing ticketing systems;
- ask a requester one focused follow-up question, wait durably, and resume
  from their answer without an operator having to reconstruct the case;
- collect relevant evidence from configured services;
- support a growing set of safe, reviewable work types: access help, incident
  triage, vendor-risk research, and systems/documentation questions;
- produce evidence-backed diagnoses and proposed actions;
- enforce administrator-defined policy and approval requirements;
- execute only explicitly authorized capabilities; and
- preserve a durable audit trail of every decision and action.

## Current scope

The current milestone is deliberately limited to **SHADOW** mode. A Jira ticket
can be ingested, Google Workspace context can be gathered, the model can ask a
focused question when the request is incomplete, and a later Jira reply resumes
the same durable investigation. When enough context exists, Wardstone produces
an evidence-backed diagnosis and proposed actions; policy evaluates each
proposal. No state-changing action is executed in this mode.

The admin console now exposes an onboarding and connector-readiness foundation:
Wardstone can start with Jira, Google, and the model unconfigured, explains the
resulting feature availability, and runs explicit read-only connection tests.
Credentials remain environment-managed and are never entered into or returned
by the browser. The next product work is bounded skills for the three proof
points: interactive helpdesk work, access management, and vendor review. Slack
remains an important requester/approval surface, but it is not a prerequisite
for the core case lifecycle.

## Jira development setup

Wardstone's Jira connector uses a dedicated Jira account with permission to
browse the target project. Configure its base URL, account
email, API token, and a separate inbound webhook secret. The application rejects
non-HTTPS Jira URLs except loopback addresses used for local development.

In SHADOW mode, questions and final investigation summaries are durable drafts
visible to operators; Wardstone never posts model-generated content to Jira.
The delivery outbox is retained for a future reviewed sending workflow.

See [development setup](docs/development.md) for the environment variables and
Jira Automation payload shape.

See [the architecture](docs/architecture.md) for design details and
[development setup](docs/development.md) for local commands.

## Product direction and MVP order

Wardstone is being built around a simple operator promise: bring the operator
in only when their judgment or authority is actually needed. A successful MVP
will demonstrate this across three kinds of work: a vague helpdesk request that
is clarified and resolved, a bounded access-management request, and a vendor
review that returns a cited risk profile.

The priority order is:

1. Durable case context and requester conversations — **implemented in this
   slice**.
2. An admin onboarding flow that scopes environment-managed credentials, tests
   read access, and explains resulting permissions — **readiness foundation
   implemented; managed credential provisioning remains future work**.
3. Bounded, testable investigation skills for helpdesk, IAM, vendor risk, and
   systems knowledge; each returns an evidence-backed work package.
4. Data classification, model-routing controls, approval/execution, and a
   transactional delivery layer for Jira and Slack.

Ticket routing is deterministic-first: ordered rules use selected Jira fields
before Wardstone asks a model for a confidence-gated intent classification.
The model can choose only an installed specialist profile and does not receive
any permission to act. Each case stores its original rule/model/fallback
decision and matching audit event; later clarification or specialist handoffs
retain that history. Operators can test Jira mappings in the console's routing
preview without creating work or calling a model. See the example policy and
[development setup](docs/development.md#jira-ticket-routing-metadata).

We will not spend the MVP on a generic chat interface, unbounded autonomous
execution, or a large catalog of shallow one-off automations. The useful unit
is a durable case with clear context, a narrow job to do, evidence, and an
explicit handoff boundary.

## Operator interfaces

Wardstone exposes two operator views backed by the same authenticated admin
API:

- a responsive web console at `/admin/` for service status, investigation
  history, connector setup/readiness, audit timelines, and approval requests;
  and
- a Charm-based terminal console with overview, investigation, and approval
  views.

Both interfaces require `WARDSTONE_OPERATOR_TOKEN`. Approval decisions are
recorded through the approval service and immutable audit timeline; neither UI
updates database state directly. The current runtime remains restricted to
**SHADOW** mode, so approval queues are normally empty until approval mode and
request dispatch are enabled.

## Status

Wardstone is at an early bootstrap stage. Interfaces and schemas are expected
to evolve before the first tagged release.

## License

Apache-2.0. See [LICENSE](LICENSE).
