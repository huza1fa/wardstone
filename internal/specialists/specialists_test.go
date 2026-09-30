package specialists

import (
	"testing"

	"github.com/wardstone-project/wardstone/internal/domain"
)

func TestDispatcherRoutesAccessTermsAndFallsBackToHelpDesk(t *testing.T) {
	t.Parallel()
	registry, err := NewRegistry(
		Profile{Name: HelpDesk, Instructions: "Help users."},
		Profile{Name: AccessManagement, Instructions: "Investigate access."},
	)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewDispatcher(registry)
	if err != nil {
		t.Fatal(err)
	}
	if got := dispatcher.Dispatch(domain.Ticket{Summary: "Need access to the finance folder"}); got.Specialist != AccessManagement || got.Classification != "access_management" {
		t.Fatalf("access decision = %+v", got)
	}
	if got := dispatcher.Dispatch(domain.Ticket{Summary: "VPN connection fails"}); got.Specialist != HelpDesk || got.Classification != "help_desk" {
		t.Fatalf("fallback decision = %+v", got)
	}
}

func TestDispatcherPrefersStructuredJiraRoutingRules(t *testing.T) {
	t.Parallel()
	registry, err := NewRegistry(
		Profile{Name: HelpDesk, Instructions: "Help users."},
		Profile{Name: VendorReview, Instructions: "Review vendors."},
		Profile{Name: AccessManagement, Instructions: "Investigate access."},
	)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewDispatcherWithRules(registry, []RouteRule{{
		Name: "vendor-security-review", Specialist: VendorReview, Classification: "vendor_review",
		Match: RouteMatch{IssueTypes: []string{"Service Request"}, Labels: []string{"vendor"}, Fields: map[string][]string{"Review type": {"Security"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	decision := dispatcher.Dispatch(domain.Ticket{
		Summary: "Need access to a vendor portal", // Keyword routing must not override authoritative Jira fields.
		Metadata: TicketMetadataForTest(),
	})
	if decision.Specialist != VendorReview || decision.Classification != "vendor_review" || decision.NeedsIntent {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestDispatcherMarksUnknownTicketsForOptionalIntentClassification(t *testing.T) {
	t.Parallel()
	registry, err := NewRegistry(Profile{Name: HelpDesk, Instructions: "Help users."})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewDispatcher(registry)
	if err != nil {
		t.Fatal(err)
	}
	decision := dispatcher.Dispatch(domain.Ticket{Summary: "My workstation feels strange"})
	if decision.Specialist != HelpDesk || !decision.NeedsIntent {
		t.Fatalf("decision = %+v", decision)
	}
}

func TicketMetadataForTest() domain.TicketMetadata {
	return domain.TicketMetadata{
		IssueType: "service request", Labels: []string{"Vendor"},
		Fields: map[string][]string{"review type": {"security"}},
	}
}

func TestRegistryCopiesProfileSlices(t *testing.T) {
	t.Parallel()
	collectors := []domain.CapabilityName{"directory.user.get"}
	profiles, err := NewRegistry(Profile{Name: HelpDesk, Instructions: "Help users.", AllowedCollectors: collectors})
	if err != nil {
		t.Fatal(err)
	}
	collectors[0] = "directory.user.delete"
	if !profiles.AllowsCollector(HelpDesk, "directory.user.get") || profiles.AllowsCollector(HelpDesk, "directory.user.delete") {
		t.Fatal("registry retained a caller-owned mutable collector slice")
	}
}
