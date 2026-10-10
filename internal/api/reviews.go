package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/wardstone-project/wardstone/internal/delivery"
	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/investigations"
)

type MessageReviewService interface {
	ReviewMessage(context.Context, domain.InvestigationID, domain.MessageID, delivery.Review, time.Time) (domain.MessageDelivery, bool, error)
	ListDeliveries(context.Context, domain.InvestigationID) ([]domain.MessageDelivery, error)
}

func WithMessageReviews(service MessageReviewService, enabled bool) Option {
	return func(s *Server) error {
		if service == nil {
			return errors.New("message review service is required")
		}
		s.reviews, s.reviewEnabled = service, enabled
		return nil
	}
}

func (s *Server) reviewMessage(w http.ResponseWriter, r *http.Request) {
	if !authorized(r, s.operatorToken) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.reviews == nil || !s.reviewEnabled || s.mode != domain.OperatingModeReview {
		writeError(w, http.StatusServiceUnavailable, "reviewed Jira sending is not enabled")
		return
	}
	defer r.Body.Close()
	var review delivery.Review
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&review); err != nil {
		writePayloadDecodeError(w, err, "invalid review payload")
		return
	}
	if err := requireEOF(decoder); err != nil {
		writePayloadDecodeError(w, err, "payload must contain one JSON object")
		return
	}
	if err := review.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	item, created, err := s.reviews.ReviewMessage(r.Context(), domain.InvestigationID(r.PathValue("id")), domain.MessageID(r.PathValue("messageID")), review, s.now())
	switch {
	case errors.Is(err, investigations.ErrNotFound):
		writeError(w, http.StatusNotFound, "draft not found")
	case errors.Is(err, delivery.ErrReviewConflict):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not review message")
	default:
		status := http.StatusOK
		if created {
			status = http.StatusAccepted
		}
		writeJSON(w, status, map[string]any{"delivery": item, "created": created})
	}
}
