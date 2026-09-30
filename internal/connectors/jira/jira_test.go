package jira

import (
	"testing"
	"time"
)

func TestNormalizePreservesStructuredRoutingMetadata(t *testing.T) {
	t.Parallel()
	ticket, err := Normalize(TicketPayload{
		ExternalID: "HELP-42", Summary: "Vendor security review", IssueType: "Service Request",
		RequestType: "Vendor review", Components: []string{"Procurement"}, Labels: []string{"vendor"},
		Fields: map[string][]string{"Review type": {"Security"}},
	}, time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if ticket.Metadata.IssueType != "Service Request" || ticket.Metadata.Fields["Review type"][0] != "Security" {
		t.Fatalf("metadata = %+v", ticket.Metadata)
	}
}
