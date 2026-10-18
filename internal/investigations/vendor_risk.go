package investigations

import (
	"encoding/json"
	"github.com/wardstone-project/wardstone/internal/domain"
)

// Only current connector selection observations choose administrator context.
func (s *Service) vendorReviewContext(current []domain.Evidence) domain.VendorReviewContext {
	context := domain.VendorReviewContext{Rubric: s.config.VendorRisk.Rubric.Clone()}
	for _, e := range current {
		if e.Source != "research" || e.Capability != "research.vendor.collect" || e.Kind != "research.request_context" {
			continue
		}
		var selection struct {
			Resource string `json:"resource"`
			Status   string `json:"status"`
		}
		if json.Unmarshal(e.Data, &selection) != nil || selection.Status != "ready" {
			continue
		}
		context.Vendor = selection.Resource
		context.Deployment = s.config.VendorRisk.Deployments[selection.Resource]
		break
	}
	return context
}
