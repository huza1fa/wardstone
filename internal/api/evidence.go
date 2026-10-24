package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/wardstone-project/wardstone/internal/admin"
	"github.com/wardstone-project/wardstone/internal/domain"
)

type ResearchEvidenceReader interface {
	GetResearchEvidence(context.Context, domain.InvestigationID, domain.EvidenceID) (admin.ResearchEvidence, error)
}

func WithResearchEvidence(reader ResearchEvidenceReader) Option {
	return func(s *Server) error {
		if reader == nil {
			return errors.New("research evidence reader is required")
		}
		s.researchEvidence = reader
		return nil
	}
}

func (s *Server) getResearchEvidence(w http.ResponseWriter, r *http.Request) {
	// Sensitive document excerpts must not enter browser/proxy caches, including
	// error responses. Webhook credentials confer no operator read authority.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !authorized(r, s.operatorToken) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.researchEvidence == nil {
		writeError(w, http.StatusServiceUnavailable, "research evidence inspection is not configured")
		return
	}
	id, evidenceID := r.PathValue("id"), r.PathValue("evidenceID")
	if len(id) > 128 || len(evidenceID) > 128 {
		writeError(w, http.StatusBadRequest, "invalid investigation or evidence ID")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	item, err := s.researchEvidence.GetResearchEvidence(ctx, domain.InvestigationID(id), domain.EvidenceID(evidenceID))
	switch {
	case errors.Is(err, admin.ErrEvidenceNotFound):
		writeError(w, http.StatusNotFound, "research evidence not found")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not load research evidence")
	case string(item.InvestigationID) != id || string(item.ID) != evidenceID:
		writeError(w, http.StatusNotFound, "research evidence not found")
	default:
		writeJSON(w, http.StatusOK, map[string]any{"evidence": item})
	}
}
