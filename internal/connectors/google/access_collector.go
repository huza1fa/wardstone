package google

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/wardstone-project/wardstone/internal/domain"
)

type AccessCollector struct {
	client    AccessClient
	resources map[string]string
}

func NewAccessCollector(client AccessClient, resources map[string]string) (*AccessCollector, error) {
	if client == nil {
		return nil, errors.New("access collector requires a client")
	}
	copy, err := ValidateAccessResources(resources)
	if err != nil {
		return nil, err
	}
	return &AccessCollector{client: client, resources: copy}, nil
}

// NewUnconfiguredAccessCollector still supplies administrator-owned selectors
// and the connector-readiness gap. It cannot perform Directory calls.
func NewUnconfiguredAccessCollector(resources map[string]string) (*AccessCollector, error) {
	copy, err := ValidateAccessResources(resources)
	if err != nil {
		return nil, err
	}
	return &AccessCollector{resources: copy}, nil
}

func (*AccessCollector) Name() domain.ConnectorName        { return Name }
func (*AccessCollector) Capability() domain.CapabilityName { return AccessInspect }

type AccessUnknown struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

// AccessEvidenceV1 observes direct membership only. Configured group mapping is
// administrator context, not a claim of effective application access or authorization.
type AccessEvidenceV1 struct {
	Version          int               `json:"version"`
	Status           string            `json:"status"`
	Resources        map[string]string `json:"configured_resources"`
	Resource         string            `json:"resource,omitempty"`
	ResourceSource   string            `json:"resource_source,omitempty"`
	GroupEmail       string            `json:"group_email,omitempty"`
	UserEmail        string            `json:"user_email,omitempty"`
	UserSource       string            `json:"user_source,omitempty"`
	Unknowns         []AccessUnknown   `json:"unknowns"`
	Clarifications   []string          `json:"clarification_requirements"`
	Escalations      []string          `json:"escalation_requirements"`
	User             *User             `json:"user,omitempty"`
	Group            *Group            `json:"group,omitempty"`
	Member           *Member           `json:"member,omitempty"`
	DirectMembership string            `json:"direct_membership"`
	Scope            string            `json:"scope"`
}

func (c *AccessCollector) Collect(ctx context.Context, ticket domain.Ticket) ([]domain.Evidence, error) {
	return c.CollectCase(ctx, ticket, nil)
}

// Messages must be durable context from authenticated connector intake. Only an
// inbound message from the ticket's own requester and source can supply a resource.
func (c *AccessCollector) CollectCase(ctx context.Context, ticket domain.Ticket, messages []domain.CaseMessage) ([]domain.Evidence, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data := AccessEvidenceV1{Version: 1, Status: "clarification_required", Resources: c.resources,
		Unknowns: []AccessUnknown{}, Clarifications: []string{}, Escalations: []string{}, DirectMembership: "unknown",
		Scope: "Configured Google group and direct USER membership only; effective application access and authorization are not assessed."}
	resource, present, malformed := accessField(ticket.Metadata.Fields, "Access resource")
	if malformed {
		data.Unknowns = append(data.Unknowns, AccessUnknown{"resource", "malformed_or_ambiguous_field"})
		data.Escalations = append(data.Escalations, accessResourceCorrection)
	} else if present {
		data.Resource, data.GroupEmail = c.resolveResource(resource)
		data.ResourceSource = "ticket_field"
		if data.Resource == "" {
			data.Unknowns = append(data.Unknowns, AccessUnknown{"resource", "unknown_resource"})
			data.Escalations = append(data.Escalations, accessResourceCorrection)
		}
	} else {
		latest := latestRequesterMessage(ticket, messages)
		if latest != nil {
			data.Resource, data.GroupEmail = c.resolveResource(strings.TrimSpace(latest.Body))
			if data.Resource != "" {
				data.ResourceSource = "requester_message"
			}
		}
		if data.Resource == "" {
			data.Unknowns = append(data.Unknowns, AccessUnknown{"resource", "missing_resource"})
			data.Clarifications = append(data.Clarifications, "Provide exactly one configured resource name as the entire requester reply.")
		}
	}
	user, explicit, invalid := accessField(ticket.Metadata.Fields, "Access user")
	if !explicit && !invalid {
		user = ticket.ReporterEmail
		data.UserSource = "reporter"
	} else {
		data.UserSource = "ticket_field"
	}
	if invalid || !validAccessEmail(user) {
		data.Unknowns = append(data.Unknowns, AccessUnknown{"user", "missing_or_malformed_target_user"})
		if explicit || invalid {
			data.Escalations = append(data.Escalations, "Operator must correct Access user to exactly one plain target-user email and submit a new case with a new external case ID; current intake deduplicates existing cases and does not refresh their metadata. An explicit Access user cannot default to the reporter or be repaired by a requester reply.")
		} else {
			data.Escalations = append(data.Escalations, "Operator must supply a valid reporter email or exactly one plain target-user email in Access user and submit a new case with a new external case ID; current intake deduplicates existing cases and does not refresh their metadata. A requester reply cannot repair the stored target user.")
		}
	} else {
		data.UserEmail = strings.ToLower(user)
	}
	if c.client == nil {
		data.Status = "operator_intervention_required"
		data.Unknowns = append(data.Unknowns, AccessUnknown{"connector", "google_not_configured"})
		data.Escalations = append(data.Escalations, "Google Workspace is not configured. An operator must configure and test Directory read access before membership can be inspected. A requester reply cannot resolve missing connector credentials.")
		data.Clarifications = []string{}
		return accessEvidence(data)
	}
	if len(data.Escalations) > 0 {
		data.Status = "operator_intervention_required"
		if len(data.Clarifications) > 0 {
			data.Clarifications = []string{"A resource is also missing. After the operator submits a corrected new case, provide exactly one configured resource name as the entire requester reply; replying alone cannot resolve this current case."}
		}
		return accessEvidence(data)
	}
	if len(data.Clarifications) > 0 {
		return accessEvidence(data)
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var userRecord User
	var groupRecord Group
	var userErr, groupErr error
	var wait sync.WaitGroup
	wait.Add(2)
	go func() { defer wait.Done(); userRecord, userErr = c.client.ReadAccessUser(ctx, data.UserEmail) }()
	go func() { defer wait.Done(); groupRecord, groupErr = c.client.GetGroup(ctx, data.GroupEmail) }()
	wait.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if userErr == nil && (!validProviderID(userRecord.ID) || !validAccessEmail(userRecord.PrimaryEmail) || !strings.EqualFold(userRecord.PrimaryEmail, data.UserEmail)) {
		userErr = errors.New("invalid user identity")
		data.Unknowns = append(data.Unknowns, AccessUnknown{"user", "invalid_response"})
	} else if userErr != nil {
		data.Unknowns = append(data.Unknowns, AccessUnknown{"user", accessFailure(userErr)})
	}
	if groupErr == nil && !validGroup(groupRecord, data.GroupEmail) {
		groupErr = errors.New("invalid group identity")
		data.Unknowns = append(data.Unknowns, AccessUnknown{"group", "invalid_response"})
	} else if groupErr != nil {
		data.Unknowns = append(data.Unknowns, AccessUnknown{"group", accessFailure(groupErr)})
	}
	if userErr == nil {
		data.User = &userRecord
	}
	if groupErr == nil {
		data.Group = &groupRecord
	}
	data.Status = "observed"
	if userErr == nil && groupErr == nil {
		member, err := c.client.GetDirectMember(ctx, groupRecord.ID, userRecord.ID)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			if accessFailure(err) == "not_found" {
				data.DirectMembership = "not_member"
			} else {
				data.Unknowns = append(data.Unknowns, AccessUnknown{"direct_membership", accessFailure(err)})
			}
		} else if !validMemberShape(member) || member.ID != userRecord.ID || !strings.EqualFold(member.Email, data.UserEmail) {
			data.Unknowns = append(data.Unknowns, AccessUnknown{"direct_membership", "invalid_response"})
		} else {
			data.Member = &member
			data.DirectMembership = "member"
		}
	}
	if len(data.Unknowns) > 0 {
		data.Status = "incomplete_observation"
	}
	return accessEvidence(data)
}

const accessResourceCorrection = "Operator must correct Access resource to exactly one configured resource name and submit a new case with a new external case ID; current intake deduplicates existing cases and does not refresh their metadata. A requester reply cannot repair this stored resource selector."

func accessEvidence(data AccessEvidenceV1) ([]domain.Evidence, error) {
	// Preserve the canonical schema even when callers construct evidence values.
	if data.Escalations == nil {
		data.Escalations = []string{}
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return []domain.Evidence{{Source: Name, Kind: "google.access_request", Summary: "Bounded access request inspection: " + data.Status, Data: encoded}}, nil
}

func (c *AccessCollector) resolveResource(value string) (string, string) {
	if !validResourceName(value) || value != strings.TrimSpace(value) {
		return "", ""
	}
	for name, group := range c.resources {
		if strings.EqualFold(name, value) {
			return name, group
		}
	}
	return "", ""
}

// Even near-matching field keys cannot silently default or be used as a second selector.
func accessField(fields map[string][]string, key string) (string, bool, bool) {
	count := 0
	var values []string
	exact := false
	for field, v := range fields {
		if strings.EqualFold(strings.TrimSpace(field), key) {
			count++
			values = v
			exact = field == key
		}
	}
	if count == 0 {
		return "", false, false
	}
	if count != 1 || !exact || len(values) != 1 {
		return "", true, true
	}
	return values[0], true, false
}

func latestRequesterMessage(ticket domain.Ticket, messages []domain.CaseMessage) *domain.CaseMessage {
	if !validAccessEmail(ticket.ReporterEmail) || ticket.Source == "" {
		return nil
	}
	var latest *domain.CaseMessage
	for i := range messages {
		message := &messages[i]
		if message.Direction != domain.MessageInbound || message.Source != ticket.Source || message.ExternalID == "" || !validAccessEmail(message.Author) || !strings.EqualFold(message.Author, ticket.ReporterEmail) {
			continue
		}
		if latest == nil || !message.CreatedAt.Before(latest.CreatedAt) {
			latest = message
		}
	}
	return latest
}
