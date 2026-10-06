# Operating model

Wardstone is a self-hosted operational team, not a chatbot or an autonomous
administrator. It accepts work from Jira, Slack, email, and other connected
sources, and treats every request as a durable work item that must be
understood before anything is acted upon.

## The control boundary

Incoming content tells Wardstone **what to investigate**. Wardstone
configuration tells it **what it may do**. Deterministic policy tells it
**what may actually happen**.

The model may be nondeterministic. Authority must not be. Ticket text, chat,
email, websites, documents, PDFs, vendor portals, and tool output are all
untrusted input. They may supply evidence, but they never alter system
instructions, permissions, policies, available capabilities, or approval
requirements.

All meaningful state-changing work is represented as a structured capability
proposal. A model may propose, for example,
`google.groups.add_member` with arguments, a reason, and cited evidence; it
cannot invoke that operation itself. The policy engine deterministically
allows, requires approval for, or denies the exact immutable proposal. The
executor performs only the authorized capability invocation and verification
records the result.

## Work lifecycle

The dispatcher owns the work item's lifecycle but is deliberately
least-privileged. It classifies the request, identifies relevant people,
systems, and resources, assembles the minimum useful initial context, routes
the work to one or more specialists, coordinates handoffs, and determines
whether the case is complete or needs escalation. It is not a universal agent
with every connector and permission.

Specialists are operational competencies, not independent personalities or
applications. Each uses the same Wardstone runtime and is defined by:

- instructions and expected structured output;
- accessible context and registered read tools;
- permitted capabilities;
- escalation and handoff criteria; and
- evidence and verification requirements.

Initial specialist domains include help desk, access management, cloud
operations, and vendor review. New specialists must be composable without
introducing a separate orchestration system. They follow least privilege: a
vendor reviewer does not receive account-mutation privileges, and a cloud
troubleshooter may inspect infrastructure without being able to alter it.

A specialist may gather evidence, query configured services, inspect
documentation and historical cases, ask a requester focused questions, and
correlate results across services without involving an administrator. It can
hand off a case when evidence reveals another domain. Handoffs retain the
original request, accumulated evidence, conversation, and audit history, so
the requester experiences one continuous interaction.

The implemented case-memory path scopes that continuity to the active role:
only same-case observations with explicit provenance for its allowed read
collectors reach the model. Legacy evidence without capability provenance is
hidden from model memory. Current observations have priority within the 128
observation / 256 KiB diagnosis-input limits; omissions and historical/stale
evidence are identified as context gaps. Broader specialists' handoff reasons
are withheld when their evidence scope exceeds the receiving role's scope.

Typical flow:

```text
Help desk → Cloud operations → Access management → Policy → Approval
          ← verification / requester update ← Executor
```

## Human attention is the scarce resource

Wardstone should quietly handle routine investigation, context gathering,
requester follow-ups, research, coordination, report preparation, and
read-only verification within its configured boundaries. It interrupts an
administrator only when authority, judgment, or unavailable information is
needed: for example a state-changing or privileged action, ambiguous security
decision, policy conflict, insufficient evidence, unexpected risk, destructive
operation, or low-confidence conclusion.

At that boundary, Wardstone presents the evidence, proposed action, expected
impact, and reason for escalation. The administrator or deterministic policy
decides; Wardstone then carries out authorized work, verifies the outcome,
updates the requester, and closes the case.

In the current SHADOW slice, the review package and requester messages are
durable operator-visible drafts. The web investigation detail separates
verified findings, assumptions, and unknowns, shows citations and available
observation provenance/timestamps, and exposes exact action arguments and policy
outcomes. SHADOW performs no requester sends or connector writes. The delivery
outbox/Jira sender and approval state machine are foundations for the next
operator-reviewed sending and approved-action pilot.

## Evidence, research, and auditability

Specialist conclusions distinguish verified evidence, external/vendor claims,
unknowns, and assumptions. Vendor reviews, for example, can cover security
documentation, certifications, DPAs, subprocessors, data handling and
residency, retention, identity controls, privacy terms, AI data-use terms,
public incidents, and alignment with internal requirements.

The current report contract is a versioned `work_package` with `summary`,
`findings` (`verified` or `assumption`, statement, evidence IDs), `unknowns`,
`escalation_reason`, `requester_summary`, and `expected_impact`. A verified finding
requires citations to observations actually supplied to the run; the label
records the model's evidence-backed finding, not independent verification of
business authorization. Mutations require every citation to be current-run
evidence, at most 15 minutes old and no more than one minute future-dated, with
freshness rechecked after the model call. Historical observations can explain a
case but cannot ground a mutation proposal.

Bounded access help uses administrator-owned `access_help.resources` to map a
named resource to a Google group. Exact `Access resource` and `Access user`
structured fields select the resource/user; an absent resource field can be
filled only by the requester's whole-reply exact resource name. The collector
inspects direct `USER` membership only. The mapping and direct membership do
not establish application access, transitive access, or approval to grant it.
See [development setup](development.md#bounded-access-help) and
[report schema and citation rules](architecture.md#bounded-case-memory-and-reports).

Every work item is durable across process restarts and may wait for a user,
approval, API response, or vendor information for minutes or days. Its audit
history preserves the request, classification and routing, specialist runs and
handoffs, tools and capabilities used, evidence, questions and replies,
structured summaries, proposed actions, policy and approval decisions,
execution and verification results, requester communication, and final
outcome. An administrator must be able to answer what happened, why it
happened, what evidence was used, who authorized it, what changed, and whether
the result was verified.

The long-term goal is not universal autonomous IT. It is to move the
administrator from processing every interruption to supervising exceptions and
exercising judgment where it is genuinely required.
