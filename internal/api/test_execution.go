package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/wardstone-project/wardstone/internal/cases"
	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/executor"
)

func WithTestExecution(store executor.Store, enabled bool, lifetime time.Duration) Option {
	return func(s *Server) error {
		if store == nil || lifetime <= 0 || lifetime > 7*24*time.Hour {
			return errors.New("test execution store and approval lifetime are required")
		}
		s.testExecution = store
		s.testExecutionEnabled = enabled
		s.testApprovalLifetime = lifetime
		return nil
	}
}
func (s *Server) testExecutionAuth(w http.ResponseWriter, r *http.Request, write bool) bool {
	if !authorized(r, s.operatorToken) {
		writeError(w, 401, "unauthorized")
		return false
	}
	if write && (s.testExecution == nil || !s.testExecutionEnabled) {
		writeError(w, 503, "synthetic execution pilot is disabled")
		return false
	}
	return true
}
func testExecutionError(w http.ResponseWriter, err error) {
	if errors.Is(err, cases.ErrInvalid) {
		writeError(w, 400, "provide valid request_id, actor, reason, and synthetic names starting with test-")
		return
	}
	if errors.Is(err, executor.ErrConflict) || errors.Is(err, cases.ErrConflict) {
		writeError(w, 409, "test action, approval, or execution changed; refresh before trying again")
		return
	}
	writeError(w, 500, "test execution command could not be completed")
}
func (s *Server) listTestExecutions(w http.ResponseWriter, r *http.Request) {
	if !s.testExecutionAuth(w, r, false) {
		return
	}
	items := []executor.Summary{}
	if s.testExecution != nil {
		var err error
		items, err = s.testExecution.ListTestExecutions(r.Context())
		if err != nil {
			testExecutionError(w, err)
			return
		}
	}
	writeJSON(w, 200, map[string]any{"enabled": s.testExecutionEnabled, "items": items})
}
func (s *Server) proposeTestExecution(w http.ResponseWriter, r *http.Request) {
	if !s.testExecutionAuth(w, r, true) {
		return
	}
	var p executor.Proposal
	if !decodeCase(w, r, &p) {
		return
	}
	result, created, err := s.testExecution.ProposeTestAction(r.Context(), p, s.testApprovalLifetime, s.now())
	if err != nil {
		testExecutionError(w, err)
		return
	}
	writeJSON(w, 202, map[string]any{"item": result, "created": created})
}
func (s *Server) queueTestExecution(w http.ResponseWriter, r *http.Request) {
	if !s.testExecutionAuth(w, r, true) {
		return
	}
	var c cases.Command
	if !decodeCase(w, r, &c) {
		return
	}
	id, err := s.testExecution.QueueTestExecution(r.Context(), domain.ApprovalID(r.PathValue("id")), c, s.now())
	if err != nil {
		testExecutionError(w, err)
		return
	}
	writeJSON(w, 202, map[string]any{"execution_id": id})
}
func (s *Server) recheckTestExecution(w http.ResponseWriter, r *http.Request) {
	if !s.testExecutionAuth(w, r, true) {
		return
	}
	var c cases.Command
	if !decodeCase(w, r, &c) {
		return
	}
	if err := s.testExecution.RecheckTestExecution(r.Context(), domain.ExecutionID(r.PathValue("id")), c, s.now()); err != nil {
		testExecutionError(w, err)
		return
	}
	writeJSON(w, 202, map[string]any{"status": "VERIFYING", "read_only": true})
}
