package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wardstone-project/wardstone/internal/agent"
	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/evaluations"
)

func TestCLIRecordedReportAndGuards(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
	}{{[]string{}, 0}, {[]string{"--models", "live-model"}, 2}, {[]string{"--max-calls", "1"}, 2}, {[]string{"--models", "fixture,fixture"}, 2}, {[]string{"--timeout", "0s"}, 2}} {
		args := append([]string{"--cases", "../../evaluations/pilot.json"}, tc.args...)
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != tc.code {
			t.Fatalf("%v code=%d want=%d stderr=%s", args, code, tc.code, errOut.String())
		}
		if tc.code == 0 {
			var r evaluations.Report
			if json.Unmarshal(out.Bytes(), &r) != nil || !r.Passed || len(r.Results) != 7 {
				t.Fatal("invalid baseline JSON")
			}
		}
	}
	path := filepath.Join(t.TempDir(), "report.json")
	var out, errOut bytes.Buffer
	if code := run([]string{"--cases", "../../evaluations/pilot.json", "--out", path}, &out, &errOut); code != 0 {
		t.Fatalf("write report code=%d stderr=%s", code, errOut.String())
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm()&0077 != 0 || out.Len() != 0 {
		t.Fatal("output report is not private or leaked to stdout")
	}
}
func TestCLIMultipleModelsThroughFixtureProviderProtocol(t *testing.T) {
	t.Setenv("WARDSTONE_AI_KEY", "")
	t.Setenv("WARDSTONE_OPENAI_API_KEY", "")
	f, err := os.Open("../../evaluations/pilot.json")
	if err != nil {
		t.Fatal(err)
	}
	d, err := evaluations.Read(f)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("decode request")
			return
		}
		var request agent.Request
		if len(body.Messages) < 2 || json.Unmarshal([]byte(body.Messages[1].Content), &request) != nil {
			t.Error("missing production request")
			return
		}
		var response agent.Result
		for _, c := range d.Cases {
			if c.Request.InvestigationID == request.InvestigationID {
				response = *c.RecordedResult
			}
		}
		if body.Model == "citation-model" {
			response.WorkPackage.Findings[0].EvidenceIDs = []domain.EvidenceID{"invented"}
		}
		if body.Model == "bad-model" {
			response.WorkPackage.Summary = "The vendor is SOC 2 certified"
		}
		content, _ := json.Marshal(map[string]any{"result": response})
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": string(content)}}}})
	}))
	defer server.Close()
	var out, errOut bytes.Buffer
	code := run([]string{"--cases", "../../evaluations/pilot.json", "--models", "good-model,bad-model,citation-model", "--live", "--structured-output", "--base-url", server.URL + "/v1"}, &out, &errOut)
	if code != 1 || calls != 21 {
		t.Fatalf("comparison code=%d calls=%d stderr=%s", code, calls, errOut.String())
	}
	var report evaluations.Report
	if json.Unmarshal(out.Bytes(), &report) != nil || report.Passed || len(report.Results) != 21 {
		t.Fatal("invalid comparison")
	}
	if !report.Results[0].Passed || report.Results[7].Passed || report.Results[7].WordingValid {
		t.Fatalf("model regression not exposed: good=%+v bad=%+v", report.Results[0], report.Results[7])
	}
	if report.Results[14].CitationValid || !strings.Contains(strings.Join(report.Results[14].Failures, ","), "invalid_citation") {
		t.Fatal("invalid citations were hidden by provider validation")
	}
	if strings.Contains(out.String(), "recorded_result") {
		t.Fatal("report copied dataset bodies")
	}
}
