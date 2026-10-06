package investigations_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wardstone-project/wardstone/internal/agent"
	"github.com/wardstone-project/wardstone/internal/connectors"
	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/investigations"
	"github.com/wardstone-project/wardstone/internal/investigations/memory"
	"github.com/wardstone-project/wardstone/internal/policy"
	"github.com/wardstone-project/wardstone/internal/specialists"
)

type validatingCollector struct {
	collectorFunc
	validate func(domain.ProposedAction, []domain.Evidence) error
}

func (c validatingCollector) ValidateProposal(action domain.ProposedAction, evidence []domain.Evidence) error {
	return c.validate(action, evidence)
}

type identityCollector struct {
	collectorFunc
	capability domain.CapabilityName
}

func (c identityCollector) Capability() domain.CapabilityName { return c.capability }

func TestInstalledCollectorRegistryOwnershipAndDuplicates(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		collectors []connectors.EvidenceCollector
		wantError  string
	}{
		{"registered owner", []connectors.EvidenceCollector{identityCollector{collectorFunc: collectorFunc{name: "test"}, capability: "test.read"}}, ""},
		{"forged owner", []connectors.EvidenceCollector{identityCollector{collectorFunc: collectorFunc{name: "other"}, capability: "test.read"}}, "does not own"},
		{"mutating collector", []connectors.EvidenceCollector{identityCollector{collectorFunc: collectorFunc{name: "test"}, capability: "test.write"}}, "read-only"},
		{"unknown even when inactive", []connectors.EvidenceCollector{identityCollector{collectorFunc: collectorFunc{name: "test"}, capability: "unregistered"}}, "unknown capability"},
		{"duplicate capability", []connectors.EvidenceCollector{identityCollector{collectorFunc: collectorFunc{name: "test"}, capability: "test.read"}, identityCollector{collectorFunc: collectorFunc{name: "test"}, capability: "test.read"}}, "duplicate"},
	} {
		t.Run(test.name, func(t *testing.T) {
			registry := registryForTest(t)
			profiles, err := specialists.NewRegistry(specialists.Profile{Name: specialists.HelpDesk, Instructions: "Clarify tickets"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = investigations.NewService(memory.NewStore(), test.collectors, modelFunc(noActionDiagnosis), registry, policy.New(domain.OperatingModeShadow, registry, nil), investigations.Config{Specialists: profiles, MaxConcurrentCollectors: 1, CollectorTimeout: time.Second, ModelTimeout: time.Second, PromptVersion: "test-v1"})
			if test.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("err=%v want=%s", err, test.wantError)
			}
		})
	}
}

func TestInactiveCollectorValidatorsSeeOnlyReferencedFrozenObservations(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"valid", "wrong reference", "nested provider mutation", "provider provenance mutation", "unlisted capability"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			store := memory.NewStore()
			validatorCalls := 0
			collector := collectorFunc{name: "directory", collect: func(context.Context, domain.Ticket) ([]domain.Evidence, error) {
				return []domain.Evidence{
					{Kind: "access", Summary: "Selected account and group", Data: json.RawMessage(`{"user":{"email":"jane@example.com"},"group":{"id":"engineering"}}`)},
					{Kind: "unrelated", Summary: "Unrelated account", Data: json.RawMessage(`{"user":{"email":"evil@example.com"},"group":{"id":"engineering"}}`)},
				}, nil
			}}
			validator := validatingCollector{collectorFunc: collectorFunc{name: "access-validator", collect: func(context.Context, domain.Ticket) ([]domain.Evidence, error) {
				t.Error("validator collector outside active profile was called")
				return nil, nil
			}}, validate: func(action domain.ProposedAction, evidence []domain.Evidence) error {
				validatorCalls++
				if len(evidence) != 1 || len(action.EvidenceIDs) != 1 || action.EvidenceIDs[0] != evidence[0].ID {
					t.Fatalf("unreferenced evidence disclosed: %+v", evidence)
				}
				if evidence[0].Source != "directory" || evidence[0].Capability != "directory.collect" || evidence[0].InvestigationID != action.InvestigationID {
					t.Fatal("provider changed authoritative provenance")
				}
				var observation struct{ User struct{ Email string } }
				if err := json.Unmarshal(evidence[0].Data, &observation); err != nil {
					t.Fatal(err)
				}
				if observation.User.Email != "jane@example.com" {
					return errors.New("private semantic mismatch for other@example.com")
				}
				return nil
			}}
			model := modelFunc(func(_ context.Context, r agent.Request) (agent.Result, error) {
				var selected, unrelated domain.Evidence
				for _, item := range r.Evidence {
					if item.Kind == "access" {
						selected = item
					} else {
						unrelated = item
					}
				}
				id := selected.ID
				if mode == "wrong reference" || mode == "nested provider mutation" {
					id = unrelated.ID
				}
				if mode == "nested provider mutation" {
					// Edit the shared RawMessage backing bytes, not just the struct.
					old := []byte("evil@example.com")
					replacement := []byte("jane@example.com")
					index := strings.Index(string(unrelated.Data), string(old))
					copy(unrelated.Data[index:index+len(old)], replacement)
				}
				if mode == "provider provenance mutation" {
					for i := range r.Evidence {
						r.Evidence[i].Source = "forged"
						r.Evidence[i].Capability = "forbidden.collect"
						r.Evidence[i].InvestigationID = "other-case"
						r.Evidence[i].ID = "forged"
					}
				}
				capability := domain.CapabilityName("test.write")
				if mode == "unlisted capability" {
					capability = "test.read"
				}
				return agent.Result{WorkPackage: &domain.WorkPackage{Version: 1, Summary: "Review access", RequesterSummary: "Pending review", Findings: []domain.Finding{{Kind: "verified", Statement: "Account selected", EvidenceIDs: []domain.EvidenceID{id}}}}, Actions: []agent.ProposedAction{{Capability: capability, Arguments: json.RawMessage(`{"user":"jane@example.com"}`), Reason: "Review", EvidenceIDs: []domain.EvidenceID{id}}}}, nil
			})
			service := newService(t, store, []connectors.EvidenceCollector{collector, validator}, model, registryForTest(t), 1, func(c *investigations.Config) {
				profiles := c.Specialists.Profiles()
				for i := range profiles {
					profiles[i].AllowedCollectors = []domain.CapabilityName{"directory.collect"}
					if profiles[i].Name == specialists.AccessManagement {
						profiles[i].AllowedCapabilities = []domain.CapabilityName{"test.write"}
					}
				}
				var err error
				c.Specialists, err = specialists.NewRegistry(profiles...)
				if err != nil {
					t.Fatal(err)
				}
			})
			id, _, err := service.Receive(ctx, ticket("VALIDATOR-FROZEN"))
			if err != nil {
				t.Fatal(err)
			}
			err = service.Run(ctx, id)
			wantSuccess := mode == "valid" || mode == "provider provenance mutation"
			if (err == nil) != wantSuccess {
				t.Fatalf("mode=%s err=%v", mode, err)
			}
			wantCalls := 1
			if mode == "unlisted capability" {
				wantCalls = 0
			}
			if validatorCalls != wantCalls {
				t.Fatalf("validator calls=%d want=%d", validatorCalls, wantCalls)
			}
			inv, _ := store.GetInvestigation(ctx, id)
			if !wantSuccess {
				if len(store.Actions(id)) != 0 || inv.WorkPackage != nil || inv.Diagnosis != "" {
					t.Fatal("rejected action or report persisted")
				}
				if strings.Contains(inv.Failure, "private") || strings.Contains(inv.Failure, "other@example.com") {
					t.Fatalf("raw semantic error persisted: %s", inv.Failure)
				}
			} else if len(store.Actions(id)) != 1 || inv.WorkPackage == nil {
				t.Fatal("validated action or report missing")
			}
		})
	}
}

func TestValidatorIsolationIncludesArgumentsAndLaterProviderProposals(t *testing.T) {
	t.Parallel()
	store := memory.NewStore()
	var providerActions []agent.ProposedAction
	collector := collectorFunc{name: "directory", collect: func(context.Context, domain.Ticket) ([]domain.Evidence, error) {
		return []domain.Evidence{{Kind: "access", Summary: "Account", Data: json.RawMessage(`{"nested":{"allowed":false}}`)}}, nil
	}}
	firstCalls, secondCalls := 0, 0
	first := validatingCollector{collectorFunc: collectorFunc{name: "first-validator"}, validate: func(action domain.ProposedAction, evidence []domain.Evidence) error {
		firstCalls++
		copy(action.Arguments, []byte(`{"user":"evil@example.com"}`))
		action.EvidenceIDs[0] = "rewritten"
		copy(evidence[0].Data, []byte(`{"nested":{"allowed":true}} `))
		evidence[0].Source = "rewritten"
		// A component can retain the provider's original output. The runtime
		// must snapshot all proposals before validating even the first one.
		copy(providerActions[1].Arguments, []byte(`{"user":"evil@example.com"}`))
		providerActions[1].EvidenceIDs[0] = "rewritten"
		return nil
	}}
	second := validatingCollector{collectorFunc: collectorFunc{name: "second-validator"}, validate: func(action domain.ProposedAction, evidence []domain.Evidence) error {
		secondCalls++
		if string(action.Arguments) != `{"user":"jane@example.com"}` || !action.DigestValid() || action.EvidenceIDs[0] != evidence[0].ID || evidence[0].Source != "directory" || string(evidence[0].Data) != `{"nested":{"allowed":false}}` {
			t.Fatalf("validator mutation escaped isolation: action=%+v evidence=%+v", action, evidence)
		}
		return nil
	}}
	model := modelFunc(func(_ context.Context, r agent.Request) (agent.Result, error) {
		providerActions = make([]agent.ProposedAction, 2)
		for i := range providerActions {
			providerActions[i] = agent.ProposedAction{Capability: "test.write", Arguments: json.RawMessage(`{"user":"jane@example.com"}`), Reason: "Review", EvidenceIDs: []domain.EvidenceID{r.Evidence[0].ID}}
		}
		return agent.Result{Diagnosis: "Review", Actions: providerActions}, nil
	})
	service := newService(t, store, []connectors.EvidenceCollector{collector, first, second}, model, registryForTest(t), 1, func(c *investigations.Config) {
		profiles := c.Specialists.Profiles()
		for i := range profiles {
			profiles[i].AllowedCollectors = []domain.CapabilityName{"directory.collect"}
		}
		var err error
		c.Specialists, err = specialists.NewRegistry(profiles...)
		if err != nil {
			t.Fatal(err)
		}
	})
	id, _, err := service.Receive(context.Background(), ticket("VALIDATOR-ISOLATION"))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if firstCalls != 2 || secondCalls != 2 {
		t.Fatalf("calls=%d/%d", firstCalls, secondCalls)
	}
	for _, evaluation := range store.Actions(id) {
		if string(evaluation.Action.Arguments) != `{"user":"jane@example.com"}` || !evaluation.Action.DigestValid() {
			t.Fatal("validator modified persisted action")
		}
	}
}
