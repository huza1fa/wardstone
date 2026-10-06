package investigations

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wardstone-project/wardstone/internal/agent"
	"github.com/wardstone-project/wardstone/internal/audit"
	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/specialists"
)

const (
	maxPromptEvidence  = 128
	maxCasePromptBytes = agent.MaxDiagnosisPromptBytes
	// Leave room for provider framing, system instructions, and truncation warnings.
	maxCaseContextBytes  = maxCasePromptBytes - (8 << 10)
	maxEvidenceAge       = 15 * time.Minute
	maxHistoryHandoffs   = 16
	maxHistoryGaps       = 32
	maxPromptMessages    = 64
	maxConversationBytes = 64 << 10
)

// A structural extension keeps contextual access collection in this same run.
type contextualCollector interface {
	CollectCase(context.Context, domain.Ticket, []domain.CaseMessage) ([]domain.Evidence, error)
}

func cloneEvidence(items []domain.Evidence) []domain.Evidence {
	copy := append([]domain.Evidence(nil), items...)
	for i := range copy {
		copy[i].Data = append(json.RawMessage(nil), items[i].Data...)
	}
	return copy
}

func cloneAction(action domain.ProposedAction) domain.ProposedAction {
	action.Arguments = append(json.RawMessage(nil), action.Arguments...)
	action.EvidenceIDs = append([]domain.EvidenceID(nil), action.EvidenceIDs...)
	return action
}

func freshObservation(item domain.Evidence, now time.Time) bool {
	return !item.ObservedAt.IsZero() && !item.ObservedAt.Before(now.Add(-maxEvidenceAge)) && !item.ObservedAt.After(now.Add(time.Minute))
}

func (s *Service) allowedObservation(item domain.Evidence, id domain.InvestigationID, profile specialists.Profile) bool {
	if item.ID == "" || item.InvestigationID != id || item.Capability == "" || !s.specialists.AllowsCollector(profile.Name, item.Capability) {
		return false
	}
	definition, err := s.registry.Get(item.Capability)
	return err == nil && definition.Effect == domain.EffectRead && definition.Connector == item.Source
}

// Only explicit capability provenance permits historical gaps to cross a
// specialist boundary. Raw connector errors never become model context.
func (s *Service) caseHistory(ctx context.Context, id domain.InvestigationID, profile specialists.Profile) ([]agent.HandoffContext, []string, error) {
	timeline, err := s.store.Timeline(ctx, id)
	if err != nil {
		return nil, nil, fmt.Errorf("load case history: %w", err)
	}
	var handoffs []agent.HandoffContext
	var gaps []string
	seen := make(map[domain.CapabilityName]bool)
	for i := len(timeline) - 1; i >= 0; i-- {
		event := timeline[i]
		switch event.Type {
		case audit.SpecialistHandedOff:
			if len(handoffs) >= maxHistoryHandoffs || len(event.Data) > 8<<10 {
				continue
			}
			var handoff agent.HandoffContext
			if json.Unmarshal(event.Data, &handoff) != nil {
				continue
			}
			from, fromOK := s.specialists.Get(handoff.From)
			_, toOK := s.specialists.Get(handoff.To)
			if !fromOK || !toOK || !s.specialists.AllowsHandoff(handoff.From, handoff.To) {
				continue
			}
			if !utf8.ValidString(handoff.Reason) || strings.ContainsRune(handoff.Reason, 0) {
				continue
			}
			handoff.Reason = boundedString(handoff.Reason, maxActionReasonSize)
			// A prior model may have put restricted observations in its free-text
			// reason. Retain the transition but withhold that text from narrower roles.
			for _, capability := range from.AllowedCollectors {
				if !s.specialists.AllowsCollector(profile.Name, capability) {
					handoff.Reason = "Prior specialist handed off this case; reason withheld due to evidence scope."
					break
				}
			}
			handoff.OccurredAt = event.OccurredAt
			handoffs = append(handoffs, handoff)
		case audit.EvidenceCollectionFailed:
			if len(gaps) >= maxHistoryGaps || len(event.Data) > 64<<10 {
				continue
			}
			var gap struct {
				Capability domain.CapabilityName `json:"capability"`
			}
			if json.Unmarshal(event.Data, &gap) != nil || gap.Capability == "" || seen[gap.Capability] || !s.specialists.AllowsCollector(profile.Name, gap.Capability) {
				continue
			}
			seen[gap.Capability] = true
			gaps = append(gaps, fmt.Sprintf("Prior collection gap for %s; historical failure is not proof of current state.", gap.Capability))
		}
	}
	for i, j := 0, len(handoffs)-1; i < j; i, j = i+1, j-1 {
		handoffs[i], handoffs[j] = handoffs[j], handoffs[i]
	}
	return handoffs, gaps, nil
}

// boundedCaseRequest gives current observations priority over historical memory.
// It never truncates evidence content, which could change the meaning of a fact.
func (s *Service) boundedCaseRequest(request agent.Request, current, prior []domain.Evidence, profile specialists.Profile, now time.Time) (agent.Request, map[domain.EvidenceID]bool, error) {
	request.Ticket.Metadata = request.Ticket.Metadata.Clone()
	request.Conversation = append([]domain.CaseMessage(nil), request.Conversation...)
	request.Warnings = append([]string(nil), request.Warnings...)
	request.Handoffs = append([]agent.HandoffContext(nil), request.Handoffs...)
	// Empty arrays make incremental array costs exact (no null-to-array delta).
	request.Evidence = make([]domain.Evidence, 0, maxPromptEvidence)
	if request.Warnings == nil {
		request.Warnings = []string{}
	}
	truncated := false
	if len(request.Conversation) > maxPromptMessages {
		request.Conversation = request.Conversation[len(request.Conversation)-maxPromptMessages:]
		truncated = true
	}
	conversationBytes := 0
	messageSizes := make([]int, len(request.Conversation))
	for i := len(request.Conversation) - 1; i >= 0; i-- {
		encoded, err := json.Marshal(request.Conversation[i])
		if err != nil {
			return agent.Request{}, nil, fmt.Errorf("encode case conversation: %w", err)
		}
		messageSizes[i] = len(encoded)
		conversationBytes += jsonArrayAppendBytes(len(encoded), len(request.Conversation)-1-i)
		if conversationBytes > maxConversationBytes {
			request.Conversation = request.Conversation[i+1:]
			messageSizes = messageSizes[i+1:]
			truncated = true
			break
		}
	}
	if request.Conversation == nil {
		request.Conversation = []domain.CaseMessage{}
	}
	conversation := request.Conversation
	request.Conversation = []domain.CaseMessage{}
	base, err := json.Marshal(request)
	if err != nil {
		return agent.Request{}, nil, fmt.Errorf("encode case context: %w", err)
	}
	encodedBytes := len(base)
	for i, size := range messageSizes {
		encodedBytes += jsonArrayAppendBytes(size, i)
	}
	// Conversation is retained newest-first in priority, chronological in output.
	for encodedBytes > maxCaseContextBytes-1024 {
		if len(conversation) == 0 {
			return agent.Request{}, nil, fmt.Errorf("case context exceeds prompt budget")
		}
		encodedBytes -= jsonArrayAppendBytes(messageSizes[0], len(conversation)-1)
		conversation = conversation[1:]
		messageSizes = messageSizes[1:]
		truncated = true
	}
	request.Conversation = conversation
	current = append([]domain.Evidence(nil), current...)
	prior = append([]domain.Evidence(nil), prior...)
	newest := func(items []domain.Evidence) {
		sort.SliceStable(items, func(i, j int) bool {
			if items[i].ObservedAt.Equal(items[j].ObservedAt) {
				return items[i].ID > items[j].ID
			}
			return items[i].ObservedAt.After(items[j].ObservedAt)
		})
	}
	newest(current)
	newest(prior)
	seen := make(map[domain.EvidenceID]bool)
	fresh := make(map[domain.EvidenceID]bool)
	for batch, items := range [][]domain.Evidence{current, prior} {
		for _, item := range items {
			if seen[item.ID] || !s.allowedObservation(item, request.InvestigationID, profile) {
				continue
			}
			seen[item.ID] = true
			if len(request.Evidence) >= maxPromptEvidence {
				truncated = true
				continue
			}
			// Invalid JSON is excluded without leaking its payload into an error.
			if !utf8.ValidString(item.Summary) || !utf8.ValidString(item.Kind) || !utf8.Valid(item.Data) || strings.ContainsRune(item.Summary, 0) || strings.ContainsRune(item.Kind, 0) {
				truncated = true
				continue
			}
			encoded, err := json.Marshal(item)
			if err != nil {
				truncated = true
				continue
			}
			addedBytes := jsonArrayAppendBytes(len(encoded), len(request.Evidence))
			warning := ""
			if batch == 1 || !freshObservation(item, now) {
				warning = fmt.Sprintf("Evidence %s observed at %s is historical or stale; use for context only, never to ground a mutation proposal.", item.ID, item.ObservedAt.UTC().Format(time.RFC3339))
				encodedWarning, _ := json.Marshal(warning)
				addedBytes += jsonArrayAppendBytes(len(encodedWarning), len(request.Warnings))
			}
			if addedBytes > maxCaseContextBytes-1024-encodedBytes {
				truncated = true
				continue
			}
			item.Data = append(json.RawMessage(nil), item.Data...)
			request.Evidence = append(request.Evidence, item)
			if warning != "" {
				request.Warnings = append(request.Warnings, warning)
			}
			encodedBytes += addedBytes
			if batch == 0 && freshObservation(item, now) {
				fresh[item.ID] = true
			}
		}
	}
	if truncated {
		warning := "Case context was bounded by count or byte budget; omitted observations cannot be cited or used for actions."
		encoded, _ := json.Marshal(warning)
		encodedBytes += jsonArrayAppendBytes(len(encoded), len(request.Warnings))
		request.Warnings = append(request.Warnings, warning)
	}
	if len(request.Evidence) == 0 {
		warning := "No allowed observations are available. Ask for a missing requester fact or explain the uncertainty; propose no actions."
		encoded, _ := json.Marshal(warning)
		encodedBytes += jsonArrayAppendBytes(len(encoded), len(request.Warnings))
		request.Warnings = append(request.Warnings, warning)
	}
	// One final marshal verifies the accounting against the actual wire encoding,
	// including UTF-8, JSON escape expansion, array separators, and warning reserve.
	encoded, err := json.Marshal(request)
	if err != nil || len(encoded) != encodedBytes || len(encoded) > maxCaseContextBytes {
		return agent.Request{}, nil, fmt.Errorf("case context exceeds prompt budget or byte accounting is invalid")
	}
	return request, fresh, nil
}

func jsonArrayAppendBytes(encodedSize, existingCount int) int {
	if existingCount > 0 {
		return encodedSize + 1
	}
	return encodedSize
}
