package specialists

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wardstone-project/wardstone/internal/domain"
)

func TestDispatcherFreeTextNeverSelectsSpecialist(t *testing.T) {
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
	for _, summary := range []string{"Need access to the finance folder", "Vendor license review", "VPN connection fails"} {
		if got := dispatcher.Dispatch(domain.Ticket{Summary: summary}); got.Specialist != HelpDesk || got.Classification != "help_desk" || got.Source != "fallback" || got.FallbackCode != "no_rule_match" {
			t.Fatalf("fallback decision = %+v", got)
		}
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
		Summary:  "Need access to a vendor portal", // Keyword routing must not override authoritative Jira fields.
		Metadata: TicketMetadataForTest(),
	})
	if decision.Specialist != VendorReview || decision.Classification != "vendor_review" || decision.Source != "rule" || decision.RuleName != "vendor-security-review" {
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
	if decision.Specialist != HelpDesk || decision.Source != "fallback" {
		t.Fatalf("decision = %+v", decision)
	}
}

func TicketMetadataForTest() domain.TicketMetadata {
	return domain.TicketMetadata{
		IssueType: "service request", Labels: []string{"Vendor"},
		Fields: map[string][]string{"Review type": {"security"}},
	}
}

func testRegistry(t testing.TB) *Registry {
	t.Helper()
	registry, err := NewRegistry(Profile{Name: HelpDesk, Instructions: "Help users."}, Profile{Name: VendorReview, Instructions: "Review vendors."}, Profile{Name: AccessManagement, Instructions: "Investigate access."})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func vendorRule() RouteRule {
	return RouteRule{Name: "vendor", Specialist: VendorReview, Classification: "vendor_review", Match: RouteMatch{
		Sources: []string{" jira "}, IssueTypes: []string{"Incident", "Service Request"},
		RequestTypes: []string{"Vendor review"}, Components: []string{"Procurement"}, Labels: []string{"Vendor"},
		Fields: map[string][]string{" Review type ": {"Security", "Privacy"}},
	}}
}

func vendorTicket() domain.Ticket {
	return domain.Ticket{Source: "JIRA", Metadata: domain.TicketMetadata{
		IssueType: " service request ", RequestType: " vendor review ", Components: []string{"Other", " PROCUREMENT "}, Labels: []string{"vendor"},
		Fields: map[string][]string{"Review type": {"privacy"}},
	}}
}

func TestDispatcherSelectorsAndPrecedence(t *testing.T) {
	t.Parallel()
	rule := vendorRule()
	catchAll := RouteRule{Name: "general-jira", Specialist: HelpDesk, Classification: "triage", Match: RouteMatch{Sources: []string{"Jira"}}}
	dispatcher, err := NewDispatcherWithRules(testRegistry(t), []RouteRule{rule, catchAll})
	if err != nil {
		t.Fatal(err)
	}
	if got := dispatcher.Dispatch(vendorTicket()); got.RuleName != "vendor" {
		t.Fatalf("precedence decision = %+v", got)
	}
	cases := []struct {
		name     string
		mutate   func(*domain.Ticket)
		wantRule string
	}{
		{"source", func(ticket *domain.Ticket) { ticket.Source = "email" }, ""},
		{"issue type", func(ticket *domain.Ticket) { ticket.Metadata.IssueType = "Task" }, "general-jira"},
		{"request type", func(ticket *domain.Ticket) { ticket.Metadata.RequestType = "Access request" }, "general-jira"},
		{"component", func(ticket *domain.Ticket) { ticket.Metadata.Components = []string{"IT"} }, "general-jira"},
		{"label", func(ticket *domain.Ticket) { ticket.Metadata.Labels = nil }, "general-jira"},
		{"field value", func(ticket *domain.Ticket) { ticket.Metadata.Fields["Review type"] = []string{"Billing"} }, "general-jira"},
		{"case sensitive field", func(ticket *domain.Ticket) { ticket.Metadata.Fields = map[string][]string{"review type": {"Privacy"}} }, "general-jira"},
		{"trimmed field", func(ticket *domain.Ticket) {
			ticket.Metadata.Fields = map[string][]string{" Review type ": {"Privacy"}}
		}, "vendor"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ticket := vendorTicket()
			tc.mutate(&ticket)
			if got := dispatcher.Dispatch(ticket); got.RuleName != tc.wantRule {
				t.Fatalf("decision = %+v, want rule %q", got, tc.wantRule)
			}
		})
	}
	reverse, err := NewDispatcherWithRules(testRegistry(t), []RouteRule{catchAll, rule})
	if err != nil {
		t.Fatal(err)
	}
	if got := reverse.Dispatch(vendorTicket()); got.RuleName != "general-jira" {
		t.Fatalf("reversed precedence = %+v", got)
	}
}

func TestDispatcherUnicodeSelectorsMatchIdenticalValues(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"İ", "Σ", "ſ", "K"} {
		t.Run(value, func(t *testing.T) {
			rule := RouteRule{Name: "unicode", Specialist: HelpDesk, Classification: "triage", Match: RouteMatch{RequestTypes: []string{" " + value + " "}}}
			dispatcher, err := NewDispatcherWithRules(testRegistry(t), []RouteRule{rule})
			if err != nil {
				t.Fatal(err)
			}
			if decision := dispatcher.Dispatch(domain.Ticket{Metadata: domain.TicketMetadata{RequestType: value}}); decision.RuleName != "unicode" {
				t.Fatalf("identical Unicode request type did not match: %+v", decision)
			}
		})
	}
}

func TestDispatcherCompilesImmutableConcurrentRules(t *testing.T) {
	t.Parallel()
	rules := []RouteRule{vendorRule()}
	dispatcher, err := NewDispatcherWithRules(testRegistry(t), rules)
	if err != nil {
		t.Fatal(err)
	}
	rules[0].Name = "changed"
	rules[0].Specialist = HelpDesk
	rules[0].Match.Sources[0] = "email"
	rules[0].Match.IssueTypes[1] = "Task"
	rules[0].Match.RequestTypes[0] = "changed"
	rules[0].Match.Components[0] = "changed"
	rules[0].Match.Labels[0] = "changed"
	rules[0].Match.Fields[" Review type "][1] = "changed"
	delete(rules[0].Match.Fields, " Review type ")
	var workers sync.WaitGroup
	for i := 0; i < 16; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for j := 0; j < 100; j++ {
				if got := dispatcher.Dispatch(vendorTicket()); got.Specialist != VendorReview || got.RuleName != "vendor" {
					t.Errorf("mutated decision = %+v", got)
					return
				}
			}
		}()
	}
	workers.Wait()
}

func TestRoutingRejectsMalformedAndUnboundedRules(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*RouteRule)
	}{
		{"empty name", func(r *RouteRule) { r.Name = " " }},
		{"long name", func(r *RouteRule) { r.Name = strings.Repeat("a", 129) }},
		{"NUL name", func(r *RouteRule) { r.Name = "vendor\x00" }},
		{"invalid UTF8 classification", func(r *RouteRule) { r.Classification = "\xff" }},
		{"unknown specialist", func(r *RouteRule) { r.Specialist = "unknown" }},
		{"empty classification", func(r *RouteRule) { r.Classification = " " }},
		{"empty match", func(r *RouteRule) { r.Match = RouteMatch{} }},
		{"empty selector", func(r *RouteRule) { r.Match.Labels = []string{} }},
		{"blank selector value", func(r *RouteRule) { r.Match.Labels = []string{" "} }},
		{"NUL selector value", func(r *RouteRule) { r.Match.Labels = []string{"vendor\x00"} }},
		{"invalid UTF8 selector value", func(r *RouteRule) { r.Match.RequestTypes = []string{"\xff"} }},
		{"long selector value", func(r *RouteRule) { r.Match.Labels = []string{strings.Repeat("a", 513)} }},
		{"many alternatives", func(r *RouteRule) {
			r.Match.Labels = make([]string, 33)
			for i := range r.Match.Labels {
				r.Match.Labels[i] = "value"
			}
		}},
		{"empty field", func(r *RouteRule) { r.Match.Fields = map[string][]string{"": {"value"}} }},
		{"NUL field", func(r *RouteRule) { r.Match.Fields = map[string][]string{"risk\x00": {"value"}} }},
		{"invalid UTF8 field", func(r *RouteRule) { r.Match.Fields = map[string][]string{"\xff": {"value"}} }},
		{"NUL field value", func(r *RouteRule) { r.Match.Fields = map[string][]string{"risk": {"high\x00"}} }},
		{"invalid UTF8 field value", func(r *RouteRule) { r.Match.Fields = map[string][]string{"risk": {"\xff"}} }},
		{"empty field values", func(r *RouteRule) { r.Match.Fields = map[string][]string{"key": {}} }},
		{"duplicate trimmed fields", func(r *RouteRule) { r.Match.Fields = map[string][]string{"key": {"a"}, " key ": {"b"}} }},
		{"many fields", func(r *RouteRule) {
			r.Match.Fields = make(map[string][]string)
			for i := 0; i < 65; i++ {
				r.Match.Fields[fmt.Sprint(i)] = []string{"value"}
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rule := vendorRule()
			tc.mutate(&rule)
			if _, err := NewDispatcherWithRules(testRegistry(t), []RouteRule{rule}); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
	if _, err := NewDispatcherWithRules(nil, nil); err == nil {
		t.Fatal("nil registry accepted")
	}
	registry, err := NewRegistry(Profile{Name: VendorReview, Instructions: "Review vendors."})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewDispatcher(registry); err == nil {
		t.Fatal("missing Help Desk accepted")
	}
	rule := vendorRule()
	duplicate := vendorRule()
	duplicate.Name = " vendor "
	if _, err := NewDispatcherWithRules(testRegistry(t), []RouteRule{rule, duplicate}); err == nil {
		t.Fatal("duplicate trimmed route accepted")
	}
	if _, err := NewDispatcherWithRules(testRegistry(t), make([]RouteRule, MaxRoutingRules+1)); err == nil {
		t.Fatal("too many rules accepted")
	}
	for _, bound := range []string{"selectors", "values"} {
		rules := make([]RouteRule, MaxRoutingRules)
		for i := range rules {
			rules[i] = RouteRule{Name: fmt.Sprint(i), Specialist: HelpDesk, Classification: "triage", Match: RouteMatch{Fields: make(map[string][]string)}}
			fieldCount, alternatives := 33, 1
			if bound == "values" {
				fieldCount, alternatives = 5, 32
			}
			for j := 0; j < fieldCount; j++ {
				values := make([]string, alternatives)
				for k := range values {
					values[k] = "value"
				}
				rules[i].Match.Fields[fmt.Sprint(j)] = values
			}
		}
		if _, err := NewDispatcherWithRules(testRegistry(t), rules); err == nil {
			t.Fatalf("total %s bound not enforced", bound)
		}
	}
}

func TestProfilesRejectInvalidContextStrings(t *testing.T) {
	t.Parallel()
	for field, set := range map[string]func(*Profile, string){
		"name":         func(p *Profile, value string) { p.Name = domain.SpecialistName(value) },
		"instructions": func(p *Profile, value string) { p.Instructions = value },
		"collector": func(p *Profile, value string) {
			p.AllowedCollectors = []domain.CapabilityName{domain.CapabilityName(value)}
		},
		"capability": func(p *Profile, value string) {
			p.AllowedCapabilities = []domain.CapabilityName{domain.CapabilityName(value)}
		},
		"handoff": func(p *Profile, value string) {
			p.HandoffTargets = []domain.SpecialistName{domain.SpecialistName(value)}
		},
	} {
		for name, value := range map[string]string{"blank": " \t\n", "invalid UTF8": "\xff", "NUL": "name\x00"} {
			t.Run(field+"/"+name, func(t *testing.T) {
				profile := Profile{Name: HelpDesk, Instructions: "Investigate requests."}
				set(&profile, value)
				if err := profile.Validate(); err == nil {
					t.Fatal("accepted invalid configured profile context")
				}
			})
		}
	}
	profile := Profile{Name: "Specialist role / 你好", Instructions: "Inspect 请求.", AllowedCapabilities: []domain.CapabilityName{"system.read"}}
	if err := profile.Validate(); err != nil {
		t.Fatalf("valid Unicode role rejected: %v", err)
	}
	profile = Profile{Name: domain.SpecialistName(strings.Repeat("a", 64)), Instructions: strings.Repeat("a", 8000)}
	if err := profile.Validate(); err != nil {
		t.Fatalf("valid byte boundaries rejected: %v", err)
	}
	profile.Instructions += "a"
	if err := profile.Validate(); err == nil {
		t.Fatal("accepted oversized instructions")
	}
}

func TestIntentConfigurationLimits(t *testing.T) {
	t.Parallel()
	if err := DefaultIntentConfig().Validate(); err != nil {
		t.Fatal(err)
	}
	for _, confidence := range []float64{0, -0.1, 1.1, math.NaN(), math.Inf(1)} {
		config := DefaultIntentConfig()
		config.MinConfidence = confidence
		if err := config.Validate(); err == nil {
			t.Fatalf("accepted confidence %v", confidence)
		}
	}
	for _, timeout := range []time.Duration{0, -time.Second, 5*time.Minute + time.Nanosecond} {
		config := DefaultIntentConfig()
		config.Timeout = timeout
		if err := config.Validate(); err == nil {
			t.Fatalf("accepted timeout %v", timeout)
		}
	}
}

func BenchmarkDispatcher(b *testing.B) {
	for _, count := range []int{1, 32, MaxRoutingRules} {
		b.Run(fmt.Sprintf("rules_%d", count), func(b *testing.B) {
			rules := make([]RouteRule, count)
			for i := range rules {
				rules[i] = vendorRule()
				rules[i].Name = fmt.Sprint(i)
				rules[i].Match.Labels = []string{fmt.Sprint(i)}
			}
			dispatcher, err := NewDispatcherWithRules(testRegistry(b), rules)
			if err != nil {
				b.Fatal(err)
			}
			ticket := vendorTicket()
			ticket.Metadata.Labels = []string{fmt.Sprint(count - 1)}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if got := dispatcher.Dispatch(ticket); got.Source != "rule" {
					b.Fatal(got)
				}
			}
		})
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
