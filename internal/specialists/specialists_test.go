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
