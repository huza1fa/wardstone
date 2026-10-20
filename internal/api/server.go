package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/wardstone-project/wardstone/internal/admin"
	"github.com/wardstone-project/wardstone/internal/audit"
	"github.com/wardstone-project/wardstone/internal/cases"
	"github.com/wardstone-project/wardstone/internal/connectors/jira"
	"github.com/wardstone-project/wardstone/internal/delivery"
	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/executor"
	"github.com/wardstone-project/wardstone/internal/investigations"
)

const maxRequestBody = 1 << 20

type InvestigationService interface {
	Receive(context.Context, domain.Ticket) (domain.InvestigationID, bool, error)
}

type InvestigationReader interface {
	GetInvestigation(context.Context, domain.InvestigationID) (domain.Investigation, error)
	Timeline(context.Context, domain.InvestigationID) ([]audit.Event, error)
}

type CaseMessageReader interface {
	ListMessages(context.Context, domain.InvestigationID) ([]domain.CaseMessage, error)
}

type ConversationService interface {
	CaseMessageReader
	ReceiveRequesterReply(context.Context, domain.ConnectorName, string, domain.CaseMessage) (domain.InvestigationID, bool, error)
}

// RoutingPreviewer evaluates ticket metadata without creating investigations or
// invoking models. Previewing uses the same operator authentication as reads.
type RoutingPreviewer interface {
	PreviewRouting(context.Context, domain.Ticket) (domain.RoutingDecision, error)
}

type SetupService interface {
	Status(context.Context) (admin.SetupStatus, error)
	Probe(context.Context, string) (admin.SetupComponent, error)
}

type Server struct {
	testExecution        executor.Store
	testExecutionEnabled bool
	testApprovalLifetime time.Duration
	cases                cases.Store
	casePrompt           string
	caseRunsEnabled      bool
	service              InvestigationService
	reader               InvestigationReader
	adminReader          AdminReader
	approvals            ApprovalService
	conversations        ConversationService
	routingPreview       RoutingPreviewer
	setup                SetupService
	recovery             MessageRecoveryService
	reviews              MessageReviewService
	reviewEnabled        bool
	mode                 domain.OperatingMode
	webhookSecret        string
	operatorToken        string
	now                  func() time.Time
}

func WithSetup(service SetupService) Option {
	return func(server *Server) error {
		if service == nil {
			return errors.New("setup service is required")
		}
		server.setup = service
		return nil
	}
}

func WithRoutingPreview(previewer RoutingPreviewer) Option {
	return func(server *Server) error {
		if previewer == nil {
			return errors.New("routing preview service is required")
		}
		server.routingPreview = previewer
		return nil
	}
}

func WithConversations(service ConversationService) Option {
	return func(server *Server) error {
		if service == nil {
			return errors.New("conversation service is required")
		}
		server.conversations = service
		return nil
	}
}

type Option func(*Server) error

func WithAdmin(reader AdminReader, approvals ApprovalService, mode domain.OperatingMode) Option {
	return func(server *Server) error {
		if reader == nil || approvals == nil {
			return errors.New("admin reader and approval service are required")
		}
		if !mode.Valid() {
			return errors.New("valid operating mode is required")
		}
		server.adminReader = reader
		server.approvals = approvals
		server.mode = mode
		return nil
	}
}

func NewServer(service InvestigationService, reader InvestigationReader, webhookSecret, operatorToken string, options ...Option) (*Server, error) {
	if reader == nil || operatorToken == "" {
		return nil, errors.New("investigation reader and operator token are required")
	}
	server := &Server{service: service, reader: reader, webhookSecret: webhookSecret, operatorToken: operatorToken, now: func() time.Time { return time.Now().UTC() }}
	for _, option := range options {
		if option == nil {
			return nil, errors.New("server option is required")
		}
		if err := option(server); err != nil {
			return nil, err
		}
	}
	return server, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /v1/admin/test-executions", s.listTestExecutions)
	mux.HandleFunc("POST /v1/admin/test-executions", s.proposeTestExecution)
	mux.HandleFunc("POST /v1/admin/test-executions/approvals/{id}/queue", s.queueTestExecution)
	mux.HandleFunc("POST /v1/admin/test-executions/{id}/verify", s.recheckTestExecution)
	mux.HandleFunc("POST /v1/admin/cases", s.createCase)
	mux.HandleFunc("GET /v1/admin/investigations/{id}/case", s.getCase)
	mux.HandleFunc("POST /v1/admin/investigations/{id}/cancel", s.cancelCase)
	mux.HandleFunc("POST /v1/admin/investigations/{id}/retry", s.retryCase)
	mux.HandleFunc("POST /v1/admin/investigations/{id}/reply", s.replyToCase)
	mux.HandleFunc("POST /v1/tickets/jira", s.receiveJira)
	mux.HandleFunc("POST /v1/tickets/jira/replies", s.receiveJiraReply)
	mux.HandleFunc("GET /v1/investigations/{id}", s.getInvestigation)
	mux.HandleFunc("GET /v1/investigations/{id}/messages", s.getMessages)
	mux.HandleFunc("POST /v1/investigations/{id}/messages/{messageID}/review", s.reviewMessage)
	mux.HandleFunc("POST /v1/investigations/{id}/messages/{messageID}/recovery", s.recoverMessage)
	mux.HandleFunc("GET /v1/investigations/{id}/timeline", s.getTimeline)
	mux.HandleFunc("GET /v1/admin/overview", s.getAdminOverview)
	mux.HandleFunc("GET /v1/admin/investigations", s.listAdminInvestigations)
	mux.HandleFunc("GET /v1/admin/approvals", s.listAdminApprovals)
	mux.HandleFunc("GET /v1/admin/setup", s.getSetup)
	mux.HandleFunc("POST /v1/admin/setup/connectors/{name}/test", s.testSetupConnector)
	mux.HandleFunc("POST /v1/admin/routing/preview", s.previewRouting)
	mux.HandleFunc("POST /v1/admin/approvals/{id}/decision", s.decideAdminApproval)
	s.registerWeb(mux)
	return mux
}

func (s *Server) receiveJiraReply(writer http.ResponseWriter, request *http.Request) {
	if s.webhookSecret == "" {
		writeError(writer, http.StatusServiceUnavailable, "Jira intake is not configured")
		return
	}
	if !s.authorized(request) {
		writeError(writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.conversations == nil {
		writeError(writer, http.StatusServiceUnavailable, "requester conversations are not configured")
		return
	}
	defer request.Body.Close()
	var payload jira.ReplyPayload
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
	ticketExternalID, message, err := jira.NormalizeReply(payload, s.now())
	if err != nil || len(message.Body) > 16000 {
		writeError(writer, http.StatusBadRequest, "external_id, comment_id, author, and a reply of at most 16000 bytes are required")
		return
	}
	id, created, err := s.conversations.ReceiveRequesterReply(request.Context(), jira.Name, ticketExternalID, message)
	if errors.Is(err, investigations.ErrNotFound) {
		writeError(writer, http.StatusNotFound, "investigation not found")
		return
	}
	if errors.Is(err, investigations.ErrNotAwaitingReply) {
		writeError(writer, http.StatusConflict, "investigation is not awaiting a requester reply")
		return
	}
	if errors.Is(err, investigations.ErrUnauthorizedReply) {
		writeError(writer, http.StatusForbidden, "reply author is not the ticket requester")
		return
	}
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "could not receive requester reply")
		return
	}
	status := http.StatusAccepted
	if !created {
		status = http.StatusOK
	}
	writeJSON(writer, status, map[string]any{"investigation_id": id, "created": created})
}

func (s *Server) health(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) receiveJira(writer http.ResponseWriter, request *http.Request) {
	if s.webhookSecret == "" {
		writeError(writer, http.StatusServiceUnavailable, "Jira intake is not configured")
		return
	}
	if !s.authorized(request) {
		writeError(writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.service == nil {
		writeError(writer, http.StatusServiceUnavailable, "Jira intake is not configured")
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
	id, created, err := s.service.Receive(request.Context(), ticket)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "could not receive ticket")
		return
	}
	status := http.StatusAccepted
	if !created {
		status = http.StatusOK
	}
	writeJSON(writer, status, map[string]any{"investigation_id": id, "created": created})
}

func (s *Server) getInvestigation(writer http.ResponseWriter, request *http.Request) {
	if !authorized(request, s.operatorToken) {
		writeError(writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	item, err := s.reader.GetInvestigation(request.Context(), domain.InvestigationID(request.PathValue("id")))
	if errors.Is(err, investigations.ErrNotFound) {
		writeError(writer, http.StatusNotFound, "investigation not found")
		return
	}
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "could not load investigation")
		return
	}
	writeJSON(writer, http.StatusOK, item)
}

func (s *Server) getTimeline(writer http.ResponseWriter, request *http.Request) {
	if !authorized(request, s.operatorToken) {
		writeError(writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	events, err := s.reader.Timeline(request.Context(), domain.InvestigationID(request.PathValue("id")))
	if errors.Is(err, investigations.ErrNotFound) {
		writeError(writer, http.StatusNotFound, "investigation not found")
		return
	}
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "could not load timeline")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"events": events})
}

func (s *Server) getMessages(writer http.ResponseWriter, request *http.Request) {
	if !authorized(request, s.operatorToken) {
		writeError(writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	var reader CaseMessageReader = s.conversations
	if reader == nil {
		reader, _ = s.reader.(CaseMessageReader)
	}
	if reader == nil {
		writeError(writer, http.StatusServiceUnavailable, "requester conversations are not configured")
		return
	}
	messages, err := reader.ListMessages(request.Context(), domain.InvestigationID(request.PathValue("id")))
	if errors.Is(err, investigations.ErrNotFound) {
		writeError(writer, http.StatusNotFound, "investigation not found")
		return
	}
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "could not load conversation")
		return
	}
	payload := map[string]any{"messages": messages, "review_enabled": s.reviewEnabled && s.mode == domain.OperatingModeReview, "recovery_enabled": s.recovery != nil && s.reviewEnabled && s.mode == domain.OperatingModeReview}
	digests := make(map[domain.MessageID]string)
	for _, message := range messages {
		if message.Direction == domain.MessageOutbound {
			digests[message.ID] = delivery.DraftDigest(message.Body)
		}
	}
	payload["draft_digests"] = digests
	if s.reviews != nil {
		deliveries, err := s.reviews.ListDeliveries(request.Context(), domain.InvestigationID(request.PathValue("id")))
		if err != nil {
			writeError(writer, http.StatusInternalServerError, "could not load message deliveries")
			return
		}
		payload["deliveries"] = deliveries
	}
	writeJSON(writer, http.StatusOK, payload)
}

func (s *Server) authorized(request *http.Request) bool {
	return authorized(request, s.webhookSecret)
}

func authorized(request *http.Request, expected string) bool {
	if expected == "" {
		return false
	}
	value := request.Header.Get("Authorization")
	provided, ok := strings.CutPrefix(value, "Bearer ")
	if !ok || len(provided) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

func requireEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return errors.New("trailing JSON value")
	}
	return nil
}

func writeError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]string{"error": message})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
