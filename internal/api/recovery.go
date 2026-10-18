package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/wardstone-project/wardstone/internal/delivery"
	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/investigations"
)

type MessageRecoveryService interface {
	Recover(context.Context, domain.InvestigationID, domain.MessageID, delivery.RecoveryRequest) (delivery.RecoveryResult, error)
}

func WithMessageRecovery(service MessageRecoveryService) Option {
	return func(s *Server) error {
		if service == nil {
			return errors.New("message recovery service is required")
		}
		s.recovery = service
		return nil
	}
}

func (s *Server) recoverMessage(w http.ResponseWriter, r *http.Request) {
	if !authorized(r, s.operatorToken) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.recovery == nil || !s.reviewEnabled || s.mode != domain.OperatingModeReview {
		writeError(w, http.StatusServiceUnavailable, "Jira delivery recovery is not enabled")
		return
	}
	defer r.Body.Close()
	var request delivery.RecoveryRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writePayloadDecodeError(w, err, "invalid recovery payload")
		return
	}
	if err := requireEOF(decoder); err != nil {
		writePayloadDecodeError(w, err, "payload must contain one JSON object")
		return
	}
	result, err := s.recovery.Recover(r.Context(), domain.InvestigationID(r.PathValue("id")), domain.MessageID(r.PathValue("messageID")), request)
	switch {
	case errors.Is(err, delivery.ErrInvalidRecovery):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, investigations.ErrNotFound):
		writeError(w, http.StatusNotFound, "reviewed message not found")
	case errors.Is(err, delivery.ErrRecoveryConflict):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not recover delivery")
	default:
		writeJSON(w, http.StatusOK, result)
	}
}
