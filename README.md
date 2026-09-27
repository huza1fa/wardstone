# Wardstone

Wardstone is a self-hosted, agentic IT operations control plane for solo
administrators and small teams. It connects to the tools a team already uses,
investigates incoming support work, gathers evidence, proposes structured
actions, evaluates those actions against administrator policy, and records an
immutable audit timeline.

Wardstone is not a chatbot, ITSM, MDM, or hosted service. Administrators retain
their infrastructure, credentials, policy, and model-provider accounts.

## Project intent

The goal is to make routine IT operations safer and easier to review without
giving an AI model unchecked access to infrastructure. Models may investigate
and propose work, while deterministic code owns policy, approval, execution,
and auditing.

Wardstone is intended to:

- ingest work from existing ticketing systems;
- collect relevant evidence from configured services;
- produce evidence-backed diagnoses and proposed actions;
- enforce administrator-defined policy and approval requirements;
- execute only explicitly authorized capabilities; and
- preserve a durable audit trail of every decision and action.

## Current scope

The first milestone is deliberately limited to **SHADOW** mode. A Jira ticket
can be ingested, Google Workspace context can be gathered, a model can produce
an evidence-backed diagnosis and proposed actions, policy evaluates each
proposal, and the full process is auditable. No state-changing action is
executed in this mode.

See [the architecture](docs/architecture.md) for design details and
[development setup](docs/development.md) for local commands.

## Status

Wardstone is at an early bootstrap stage. Interfaces and schemas are expected
to evolve before the first tagged release.

## License

Apache-2.0. See [LICENSE](LICENSE).
