package api

import (
	"errors"
	"net/http"

	"github.com/wardstone-project/wardstone/internal/readiness"
)

func (s *Server) getSetup(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	if !s.authorizeSetup(writer, request) {
		return
	}
	status, err := s.setup.Status(request.Context())
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "could not load setup status")
		return
	}
	writeJSON(writer, http.StatusOK, status)
}

func (s *Server) testSetupConnector(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	if !s.authorizeSetup(writer, request) {
		return
	}
	component, err := s.setup.Probe(request.Context(), request.PathValue("name"))
	if err != nil {
		switch {
		case errors.Is(err, readiness.ErrNotFound):
			writeError(writer, http.StatusNotFound, "setup component not found")
		case errors.Is(err, readiness.ErrNotConfigured):
			writeError(writer, http.StatusConflict, "setup component is not configured")
		case errors.Is(err, readiness.ErrNotProbeable):
			writeError(writer, http.StatusConflict, "setup component does not support a live test")
		case errors.Is(err, readiness.ErrProbeRunning):
			writeError(writer, http.StatusConflict, "setup component test is already running")
		case errors.Is(err, readiness.ErrProbeThrottled):
			writeError(writer, http.StatusTooManyRequests, "setup component test was run too recently")
		default:
			writeError(writer, http.StatusInternalServerError, "could not test setup component")
		}
		return
	}
	status, err := s.setup.Status(request.Context())
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "could not refresh setup status")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"component": component, "setup": status})
}

func (s *Server) authorizeSetup(writer http.ResponseWriter, request *http.Request) bool {
	if !authorized(request, s.operatorToken) {
		writeError(writer, http.StatusUnauthorized, "unauthorized")
		return false
	}
	if s.setup == nil {
		writeError(writer, http.StatusServiceUnavailable, "setup API is not configured")
		return false
	}
	return true
}
