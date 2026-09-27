package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	adminmodel "github.com/wardstone-project/wardstone/internal/admin"
	"github.com/wardstone-project/wardstone/internal/domain"
)

func TestAdminEndpointsRequireOperatorToken(t *testing.T) {
	t.Parallel()
	server := newAdminTestServer(t, &fakeAdminReader{}, &fakeApprovalService{})
	for _, path := range []string{"/v1/admin/overview", "/v1/admin/investigations", "/v1/admin/approvals"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Errorf("%s status = %d, want 401", path, response.Code)
		}
	}
}

func TestAdminOverviewIncludesRuntimeStatusAndMode(t *testing.T) {
	t.Parallel()
	reader := &fakeAdminReader{overview: adminmodel.Overview{
		GeneratedAt:    time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC),
		Investigations: adminmodel.InvestigationCounts{Total: 4, Running: 1},
		Approvals:      adminmodel.ApprovalCounts{Pending: 2},
	}}
	server := newAdminTestServer(t, reader, &fakeApprovalService{})
	request := operatorRequest(http.MethodGet, "/v1/admin/overview", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var result adminmodel.Overview
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "ok" || result.Mode != domain.OperatingModeShadow || result.Investigations.Total != 4 || result.Approvals.Pending != 2 {
		t.Fatalf("unexpected overview: %+v", result)
	}
}

func TestAdminInvestigationLimitValidation(t *testing.T) {
	t.Parallel()
	reader := &fakeAdminReader{}
	server := newAdminTestServer(t, reader, &fakeApprovalService{})
	request := operatorRequest(http.MethodGet, "/v1/admin/investigations?limit=101", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
	request = operatorRequest(http.MethodGet, "/v1/admin/investigations?limit=100", nil)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || reader.limit != 100 {
		t.Fatalf("status = %d, limit = %d; want 200, 100", response.Code, reader.limit)
	}
}

func TestAdminApprovalDecisionUsesApprovalService(t *testing.T) {
	t.Parallel()
	approvals := &fakeApprovalService{}
	server := newAdminTestServer(t, &fakeAdminReader{}, approvals)
	body := bytes.NewBufferString(`{"decision":"granted","actor":"operator@example.com"}`)
	request := operatorRequest(http.MethodPost, "/v1/admin/approvals/apr_1/decision", body)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	if approvals.id != "apr_1" || approvals.decision != domain.ApprovalGranted || approvals.actor != "operator@example.com" {
		t.Fatalf("unexpected decision call: %+v", approvals)
	}
}

func newAdminTestServer(t *testing.T, reader AdminReader, approvalService ApprovalService) *Server {
	t.Helper()
	server, err := NewServer(fakeService{}, fakeReader{}, "webhook-secret", "operator-secret",
		WithAdmin(reader, approvalService, domain.OperatingModeShadow))
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func operatorRequest(method, target string, body *bytes.Buffer) *http.Request {
	var request *http.Request
	if body == nil {
		request = httptest.NewRequest(method, target, nil)
	} else {
		request = httptest.NewRequest(method, target, body)
	}
	request.Header.Set("Authorization", "Bearer operator-secret")
	return request
}

type fakeAdminReader struct {
	overview adminmodel.Overview
	limit    int
	status   domain.ApprovalStatus
}

func (f *fakeAdminReader) Overview(context.Context) (adminmodel.Overview, error) {
	return f.overview, nil
}

func (f *fakeAdminReader) ListInvestigations(_ context.Context, limit int) ([]adminmodel.InvestigationSummary, error) {
	f.limit = limit
	return []adminmodel.InvestigationSummary{}, nil
}

func (f *fakeAdminReader) ListApprovals(_ context.Context, status domain.ApprovalStatus) ([]adminmodel.ApprovalSummary, error) {
	f.status = status
	return []adminmodel.ApprovalSummary{}, nil
}

type fakeApprovalService struct {
	id       domain.ApprovalID
	decision domain.ApprovalStatus
	actor    string
}

func (f *fakeApprovalService) Decide(_ context.Context, id domain.ApprovalID, decision domain.ApprovalStatus, actor string) (domain.Approval, error) {
	f.id, f.decision, f.actor = id, decision, actor
	return domain.Approval{ID: id, Status: decision, Actor: actor}, nil
}
