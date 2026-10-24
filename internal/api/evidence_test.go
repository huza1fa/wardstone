package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wardstone-project/wardstone/internal/admin"
	"github.com/wardstone-project/wardstone/internal/domain"
)

type evidenceReaderFunc func(context.Context, domain.InvestigationID, domain.EvidenceID) (admin.ResearchEvidence, error)

func (f evidenceReaderFunc) GetResearchEvidence(ctx context.Context, id domain.InvestigationID, evidenceID domain.EvidenceID) (admin.ResearchEvidence, error) {
	return f(ctx, id, evidenceID)
}

func TestResearchEvidenceAPI(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, token, path string
		status            int
		reader            bool
		err               error
		mismatch          bool
	}{
		{name: "operator", token: "operator-secret", status: 200, reader: true},
		{name: "anonymous", status: 401},
		{name: "webhook is not operator", token: "webhook-secret", status: 401, reader: true},
		{name: "not configured", token: "operator-secret", status: 503},
		{name: "missing", token: "operator-secret", status: 404, reader: true, err: admin.ErrEvidenceNotFound},
		{name: "redacted backend failure", token: "operator-secret", status: 500, reader: true, err: errors.New("secret database credentials")},
		{name: "wrong attempt", token: "operator-secret", status: 404, reader: true, mismatch: true},
		{name: "oversized identifier", token: "operator-secret", path: "/v1/investigations/" + strings.Repeat("i", 129) + "/evidence/evidence-1", status: 400, reader: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			reader := evidenceReaderFunc(func(ctx context.Context, id domain.InvestigationID, evidenceID domain.EvidenceID) (admin.ResearchEvidence, error) {
				calls++
				if id != "inv-1" || evidenceID != "evidence-1" {
					t.Fatalf("reader arguments = %s %s", id, evidenceID)
				}
				if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 5*time.Second {
					t.Fatal("read deadline missing")
				}
				if test.mismatch {
					id = "another-attempt"
				}
				return admin.ResearchEvidence{ID: evidenceID, InvestigationID: id, Document: admin.ResearchDocument{Content: "<script>untrusted source</script>"}}, test.err
			})
			var options []Option
			if test.reader {
				options = append(options, WithResearchEvidence(reader))
			}
			server, err := NewServer(fakeService{}, fakeReader{}, "webhook-secret", "operator-secret", options...)
			if err != nil {
				t.Fatal(err)
			}
			path := test.path
			if path == "" {
				path = "/v1/investigations/inv-1/evidence/evidence-1"
			}
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.Header.Set("Authorization", "Bearer "+test.token)
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != test.status || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatalf("response = %d %v %s", response.Code, response.Header(), response.Body.String())
			}
			if strings.Contains(response.Body.String(), "secret") || strings.Contains(response.Body.String(), "<script>") {
				t.Fatalf("unescaped source or backend failure leaked: %s", response.Body.String())
			}
			wantCalls := 0
			if test.reader && test.status != 401 && test.status != 400 {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatalf("reader called %d times, want %d", calls, wantCalls)
			}
		})
	}
	if _, err := NewServer(fakeService{}, fakeReader{}, "webhook-secret", "operator-secret", WithResearchEvidence(nil)); err == nil {
		t.Fatal("nil reader accepted")
	}
}
