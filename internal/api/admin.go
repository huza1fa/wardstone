package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/wardstone-project/wardstone/internal/admin"
	"github.com/wardstone-project/wardstone/internal/approvals"
	"github.com/wardstone-project/wardstone/internal/domain"
)

const (
	defaultInvestigationLimit = 50
	maxInvestigationLimit     = 100
)

type AdminReader interface {
	Overview(context.Context) (admin.Overview, error)
	ListInvestigations(context.Context, int) ([]admin.InvestigationSummary, error)
	ListApprovals(context.Context, domain.ApprovalStatus) ([]admin.ApprovalSummary, error)
}

type ApprovalService interface {
	Decide(context.Context, domain.ApprovalID, domain.ApprovalStatus, string) (domain.Approval, error)
}

func (s *Server) getAdminOverview(writer http.ResponseWriter, request *http.Request) {
	if !s.authorizeAdmin(writer, request) {
		return
	}
	result, err := s.adminReader.Overview(request.Context())
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "could not load admin overview")
		return
	}
	result.Status = "ok"
	result.Mode = s.mode
	writeJSON(writer, http.StatusOK, result)
}

func (s *Server) listAdminInvestigations(writer http.ResponseWriter, request *http.Request) {
	if !s.authorizeAdmin(writer, request) {
		return
	}
	limit := defaultInvestigationLimit
	if value := request.URL.Query().Get("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > maxInvestigationLimit {
			writeError(writer, http.StatusBadRequest, "limit must be an integer from 1 to 100")
			return
		}
		limit = parsed
	}
	items, err := s.adminReader.ListInvestigations(request.Context(), limit)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "could not load investigations")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"investigations": items, "limit": limit})
}

func (s *Server) listAdminApprovals(writer http.ResponseWriter, request *http.Request) {
	if !s.authorizeAdmin(writer, request) {
		return
	}
	status := domain.ApprovalStatus(strings.ToUpper(request.URL.Query().Get("status")))
	if status != "" && status != domain.ApprovalPending && status != domain.ApprovalGranted &&
		status != domain.ApprovalDenied && status != domain.ApprovalExpired {
		writeError(writer, http.StatusBadRequest, "status must be PENDING, GRANTED, DENIED, or EXPIRED")
		return
	}
	items, err := s.adminReader.ListApprovals(request.Context(), status)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "could not load approvals")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"approvals": items, "status": status})
}

func (s *Server) decideAdminApproval(writer http.ResponseWriter, request *http.Request) {
	if !s.authorizeAdmin(writer, request) {
		return
	}
	defer request.Body.Close()
	var input struct {
		Decision domain.ApprovalStatus `json:"decision"`
		Actor    string                `json:"actor"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, maxRequestBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	if err := requireEOF(decoder); err != nil {
		writeError(writer, http.StatusBadRequest, "payload must contain one JSON object")
		return
	}
	input.Decision = domain.ApprovalStatus(strings.ToUpper(string(input.Decision)))
	item, err := s.approvals.Decide(request.Context(), domain.ApprovalID(request.PathValue("id")), input.Decision, input.Actor)
	if err != nil {
		switch {
		case errors.Is(err, approvals.ErrNotFound):
			writeError(writer, http.StatusNotFound, err.Error())
		case errors.Is(err, approvals.ErrAlreadyDecided), errors.Is(err, approvals.ErrExpired):
			writeError(writer, http.StatusConflict, err.Error())
		case errors.Is(err, approvals.ErrInvalidDecision), errors.Is(err, approvals.ErrInvalidActor):
			writeError(writer, http.StatusBadRequest, err.Error())
		default:
			writeError(writer, http.StatusInternalServerError, "could not decide approval")
		}
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"approval": item})
}

func (s *Server) authorizeAdmin(writer http.ResponseWriter, request *http.Request) bool {
	if !authorized(request, s.operatorToken) {
		writeError(writer, http.StatusUnauthorized, "unauthorized")
		return false
	}
	if s.adminReader == nil || s.approvals == nil {
		writeError(writer, http.StatusServiceUnavailable, "admin API is not configured")
		return false
	}
	return true
}
