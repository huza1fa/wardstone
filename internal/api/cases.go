package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/wardstone-project/wardstone/internal/cases"
	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/investigations"
)

func WithCases(store cases.Store, prompt string, enabled bool) Option {
	return func(s *Server) error {
		if store == nil || prompt == "" {
			return errors.New("case store and prompt version are required")
		}
		s.cases = store
		s.casePrompt = prompt
		s.caseRunsEnabled = enabled
		return nil
	}
}
func (s *Server) caseAuth(w http.ResponseWriter, r *http.Request, needsModel bool) bool {
	if !authorized(r, s.operatorToken) {
		writeError(w, 401, "unauthorized")
		return false
	}
	if s.cases == nil || (needsModel && !s.caseRunsEnabled) {
		writeError(w, 503, "case controls or model worker are not configured")
		return false
	}
	return true
}
func decodeCase(w http.ResponseWriter, r *http.Request, out any) bool {
	defer r.Body.Close()
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		writePayloadDecodeError(w, err, "invalid case command")
		return false
	}
	if err := requireEOF(d); err != nil {
		writePayloadDecodeError(w, err, "payload must contain one JSON object")
		return false
	}
	return true
}
func caseError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, cases.ErrInvalid):
		writeError(w, 400, "provide a request_id (8–128 letters, digits, hyphens or underscores), actor, reason, and valid case fields")
	case errors.Is(err, cases.ErrConflict):
		writeError(w, 409, "case changed, already has a retry, reached 100 attempts, or request ID was reused with different input; refresh before trying again")
	case errors.Is(err, investigations.ErrNotFound):
		writeError(w, 404, "investigation not found")
	default:
		writeError(w, 500, "case command could not be completed")
	}
}
func (s *Server) createCase(w http.ResponseWriter, r *http.Request) {
	if !s.caseAuth(w, r, true) {
		return
	}
	var c cases.Create
	if !decodeCase(w, r, &c) {
		return
	}
	id, created, err := s.cases.CreateCase(r.Context(), c, s.casePrompt, s.now())
	if err != nil {
		caseError(w, err)
		return
	}
	writeJSON(w, 202, map[string]any{"investigation_id": id, "created": created})
}
func (s *Server) cancelCase(w http.ResponseWriter, r *http.Request) {
	if !s.caseAuth(w, r, false) {
		return
	}
	var c cases.Command
	if !decodeCase(w, r, &c) {
		return
	}
	id := domain.InvestigationID(r.PathValue("id"))
	if err := s.cases.CancelCase(r.Context(), id, c, s.now()); err != nil {
		caseError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"investigation_id": id, "status": "CANCELLED"})
}
func (s *Server) retryCase(w http.ResponseWriter, r *http.Request) {
	if !s.caseAuth(w, r, true) {
		return
	}
	var c cases.Command
	if !decodeCase(w, r, &c) {
		return
	}
	id, created, err := s.cases.RetryCase(r.Context(), domain.InvestigationID(r.PathValue("id")), c, s.casePrompt, s.now())
	if err != nil {
		caseError(w, err)
		return
	}
	writeJSON(w, 202, map[string]any{"investigation_id": id, "created": created})
}
func (s *Server) replyToCase(w http.ResponseWriter, r *http.Request) {
	if !s.caseAuth(w, r, true) {
		return
	}
	var c cases.Reply
	if !decodeCase(w, r, &c) {
		return
	}
	created, err := s.cases.ReplyToCase(r.Context(), domain.InvestigationID(r.PathValue("id")), c, s.now())
	if err != nil {
		caseError(w, err)
		return
	}
	writeJSON(w, 202, map[string]any{"created": created})
}
func (s *Server) getCase(w http.ResponseWriter, r *http.Request) {
	if !s.caseAuth(w, r, false) {
		return
	}
	id := domain.InvestigationID(r.PathValue("id"))
	attempts, err := s.cases.CaseAttempts(r.Context(), id)
	if err != nil {
		caseError(w, err)
		return
	}
	ticket, err := s.cases.GetTicket(r.Context(), id)
	if err != nil {
		caseError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"attempts": attempts, "ticket": ticket, "runs_enabled": s.caseRunsEnabled})
}
