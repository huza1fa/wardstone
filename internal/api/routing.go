package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/wardstone-project/wardstone/internal/connectors/jira"
)

func (s *Server) previewRouting(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	if !authorized(request, s.operatorToken) {
		writeError(writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.routingPreview == nil {
		writeError(writer, http.StatusServiceUnavailable, "routing preview is not configured")
		return
	}
	defer request.Body.Close()
	var payload jira.TicketPayload
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, maxRequestBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		writePayloadDecodeError(writer, err, "invalid JSON payload")
		return
	}
	if err := requireEOF(decoder); err != nil {
		writePayloadDecodeError(writer, err, "payload must contain one JSON object")
		return
	}
	ticket, err := jira.Normalize(payload, s.now())
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	decision, err := s.routingPreview.PreviewRouting(request.Context(), ticket)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "could not preview ticket routing")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"routing": decision})
}

func writePayloadDecodeError(writer http.ResponseWriter, err error, message string) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(writer, http.StatusRequestEntityTooLarge, "payload exceeds the 1 MiB limit")
		return
	}
	writeError(writer, http.StatusBadRequest, message)
}
