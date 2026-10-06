package google

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"github.com/wardstone-project/wardstone/internal/domain"
)

var (
	errAccessRemoval    = errors.New("google access proposal: membership removal is outside the bounded access-help workflow")
	errAccessArguments  = errors.New("google access proposal: invalid membership arguments")
	errAccessCitation   = errors.New("google access proposal: a current access inspection citation is required")
	errAccessInspection = errors.New("google access proposal: cited access inspection is incomplete or does not match the configured target")
	errAccessConflict   = errors.New("google access proposal: conflicting cited access inspections")
)

// ValidateProposal binds group additions to cited current observations. The core
// must call this on every installed collector, independently of the active
// profile's inspection allowlist, after capability/shape/allowlist checks. The
// supplied evidence must already exclude historical observations; this method
// performs no I/O and does not replace policy or approval evaluation.
func (c *AccessCollector) ValidateProposal(action domain.ProposedAction, evidence []domain.Evidence) error {
	if action.Capability == GroupsRemoveMember {
		return errAccessRemoval
	}
	if action.Capability != GroupsAddMember {
		return nil
	}
	var args struct {
		Group string `json:"group"`
		User  string `json:"user"`
	}
	// Recheck defensively, including duplicate keys, unknown fields, plain
	// addresses, and exactly one bounded JSON object. Never return parser data.
	if decodeBoundedAccessObject(bytes.NewReader(action.Arguments), &args, 4096) != nil ||
		decodeStrict(action.Arguments, &args) != nil || !validAccessEmail(args.Group) || !validAccessEmail(args.User) {
		return errAccessArguments
	}
	if c == nil || len(c.resources) == 0 {
		return errAccessInspection
	}
	refs := make(map[domain.EvidenceID]bool, len(action.EvidenceIDs))
	for _, id := range action.EvidenceIDs {
		if id != "" {
			refs[id] = true
		}
	}
	seen := make(map[domain.EvidenceID]bool)
	var selected *AccessEvidenceV1
	for _, item := range evidence {
		if !refs[item.ID] {
			continue
		}
		// A citation must identify one supplied record, not ambiguous records
		// sharing an ID (even if one claims a different kind or provenance).
		if seen[item.ID] {
			return errAccessConflict
		}
		seen[item.ID] = true
		if item.Capability != AccessInspect || item.Source != Name || item.Kind != "google.access_request" {
			continue
		}
		var inspection AccessEvidenceV1
		if decodeBoundedAccessObject(bytes.NewReader(item.Data), &inspection, 128<<10) != nil ||
			decodeStrict(item.Data, &inspection) != nil || !c.validProposalInspection(inspection, args.Group, args.User) ||
			!explicitNonsuspendedUser(item.Data) {
			return errAccessInspection
		}
		if selected != nil && (selected.Resource != inspection.Resource || selected.User.ID != inspection.User.ID || selected.Group.ID != inspection.Group.ID) {
			return errAccessConflict
		}
		selected = &inspection
	}
	if selected == nil {
		return errAccessCitation
	}
	return nil
}

func (c *AccessCollector) validProposalInspection(data AccessEvidenceV1, groupEmail, userEmail string) bool {
	if data.Version != 1 || data.Status != "observed" || data.Unknowns == nil || len(data.Unknowns) != 0 ||
		data.Clarifications == nil || len(data.Clarifications) != 0 || data.Escalations == nil || len(data.Escalations) != 0 ||
		data.DirectMembership != "not_member" || data.Member != nil ||
		data.User == nil || data.Group == nil || data.User.Suspended {
		return false
	}
	// Inspection output uses the canonical configured name, not a new alias or
	// a model-selected group. Its explicit mapping context must also be intact.
	configuredGroup, configured := c.resources[data.Resource]
	if !configured || len(data.Resources) != len(c.resources) {
		return false
	}
	for resource, group := range c.resources {
		if !validAccessEmail(data.Resources[resource]) || !strings.EqualFold(data.Resources[resource], group) {
			return false
		}
	}
	if !validAccessEmail(data.GroupEmail) || !strings.EqualFold(data.GroupEmail, configuredGroup) || !strings.EqualFold(groupEmail, configuredGroup) ||
		!validAccessEmail(data.UserEmail) || !strings.EqualFold(data.UserEmail, userEmail) ||
		!validProviderID(data.User.ID) || !validAccessEmail(data.User.PrimaryEmail) || !strings.EqualFold(data.User.PrimaryEmail, data.UserEmail) ||
		!validGroup(*data.Group, configuredGroup) {
		return false
	}
	return (data.ResourceSource == "ticket_field" || data.ResourceSource == "requester_message") &&
		(data.UserSource == "ticket_field" || data.UserSource == "reporter")
}

// A missing/null suspended field must not silently become a false bool. Also
// reject duplicate nested principal/group identities instead of last-value wins.
func explicitNonsuspendedUser(data json.RawMessage) bool {
	var objects struct {
		User      json.RawMessage `json:"user"`
		Group     json.RawMessage `json:"group"`
		Resources json.RawMessage `json:"configured_resources"`
	}
	if json.Unmarshal(data, &objects) != nil {
		return false
	}
	var user struct {
		Suspended *bool `json:"suspended"`
	}
	var group Group
	var resources map[string]string
	return decodeBoundedAccessObject(bytes.NewReader(objects.User), &user, 128<<10) == nil && user.Suspended != nil && !*user.Suspended &&
		decodeBoundedAccessObject(bytes.NewReader(objects.Group), &group, 128<<10) == nil &&
		decodeBoundedAccessObject(bytes.NewReader(objects.Resources), &resources, 128<<10) == nil
}
