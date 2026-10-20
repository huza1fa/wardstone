// wardstone-eval compares saved final-report cases without collecting evidence,
// creating investigations, sending requester messages, or executing proposals.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/wardstone-project/wardstone/internal/agent"
	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/evaluations"
	"github.com/wardstone-project/wardstone/internal/models"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }
func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("wardstone-eval", flag.ContinueOnError)
	flags.SetOutput(stderr)
	datasetPath := flags.String("cases", "evaluations/pilot.json", "saved version-1 final-report dataset")
	modelNames := flags.String("models", "fixture", "comma-separated model IDs; fixture uses recorded results")
	structured := flags.Bool("structured-output", false, "request schema-constrained results from compatible providers")
	live := flags.Bool("live", false, "allow billable provider calls for non-fixture models")
	baseURL := flags.String("base-url", "", "OpenAI-compatible endpoint; otherwise use configured model environment")
	timeout := flags.Duration("timeout", 60*time.Second, "deadline per provider call (up to 5m)")
	maxCalls := flags.Int("max-calls", 100, "maximum case/model calls (1–200)")
	include := flags.Bool("include-responses", false, "include model response bodies in output")
	output := flags.String("out", "", "write a private JSON report atomically instead of stdout")
	if flags.Parse(args) != nil {
		return 2
	}
	if flags.NArg() != 0 || *timeout <= 0 || *timeout > 5*time.Minute || *maxCalls < 1 || *maxCalls > 200 {
		fmt.Fprintln(stderr, "invalid evaluation arguments")
		return 2
	}
	file, err := os.Open(*datasetPath)
	if err != nil {
		fmt.Fprintln(stderr, "could not open evaluation dataset")
		return 2
	}
	raw, err := io.ReadAll(io.LimitReader(file, evaluations.MaxDatasetBytes+1))
	_ = file.Close()
	if err != nil {
		fmt.Fprintln(stderr, "could not read evaluation dataset")
		return 2
	}
	dataset, err := evaluations.Read(strings.NewReader(string(raw)))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	names := strings.Split(*modelNames, ",")
	if len(names)*len(dataset.Cases) > *maxCalls {
		fmt.Fprintln(stderr, "case/model count exceeds --max-calls")
		return 2
	}
	providers := []agent.ModelProvider{}
	seen := map[string]bool{}
	for _, rawName := range names {
		name := strings.TrimSpace(rawName)
		if name == "" || len(name) > 256 || seen[name] {
			fmt.Fprintln(stderr, "model IDs must be nonempty, bounded, and unique")
			return 2
		}
		seen[name] = true
		if name == "fixture" {
			recorded := evaluations.RecordedProvider{Results: map[domain.InvestigationID]agent.Result{}}
			for _, c := range dataset.Cases {
				if c.RecordedResult == nil {
					fmt.Fprintln(stderr, "fixture model requires a recorded_result for every case")
					return 2
				}
				recorded.Results[c.Request.InvestigationID] = *c.RecordedResult
			}
			providers = append(providers, recorded)
			continue
		}
		if !*live {
			fmt.Fprintln(stderr, "non-fixture models require --live to permit provider calls")
			return 2
		}
		endpoint := *baseURL
		if endpoint == "" {
			endpoint = os.Getenv("WARDSTONE_OPENAI_BASE_URL")
		}
		key := os.Getenv("WARDSTONE_OPENAI_API_KEY")
		if shortcut := os.Getenv("WARDSTONE_AI_KEY"); key == "" && shortcut != "" {
			if endpoint == "" || endpoint == "https://openrouter.ai/api/v1" {
				endpoint, key = "https://openrouter.ai/api/v1", shortcut
			} else {
				fmt.Fprintln(stderr, "WARDSTONE_AI_KEY requires OpenRouter; use WARDSTONE_OPENAI_API_KEY for another endpoint")
				return 2
			}
		}
		if endpoint == "" {
			endpoint = "https://api.openai.com/v1"
		}
		provider, e := models.NewOpenAICompatible(endpoint, key, name, &http.Client{Timeout: *timeout}, models.Options{StructuredOutput: *structured})
		if e != nil {
			fmt.Fprintln(stderr, "invalid model endpoint or model configuration")
			return 2
		}
		providers = append(providers, evaluationProvider{provider})
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	hash := sha256.Sum256(raw)
	report := evaluations.Report{Version: 1, DatasetSHA256: hex.EncodeToString(hash[:]), Passed: true, Results: []evaluations.Result{}}
	for _, provider := range providers {
		for _, c := range dataset.Cases {
			if ctx.Err() != nil {
				fmt.Fprintln(stderr, "evaluation interrupted")
				return 1
			}
			result := evaluations.Run(ctx, c, provider, *timeout, *include)
			report.Results = append(report.Results, result)
			report.Passed = report.Passed && result.Passed
		}
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fmt.Fprintln(stderr, "could not encode evaluation report")
		return 2
	}
	encoded = append(encoded, '\n')
	if *output != "" {
		if err = writeReport(*output, encoded); err != nil {
			fmt.Fprintln(stderr, "could not write evaluation report")
			return 2
		}
	} else {
		if _, err = stdout.Write(encoded); err != nil {
			return 2
		}
	}
	if !report.Passed {
		return 1
	}
	return 0
}
func writeReport(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".wardstone-eval-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err = file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// Grade the parsed output before the live provider rejects invalid citations.
// evaluations.Run still applies the shared production report boundary.
type evaluationProvider struct{ *models.OpenAICompatible }

func (p evaluationProvider) Diagnose(ctx context.Context, r agent.Request) (agent.Result, error) {
	return p.GenerateForEvaluation(ctx, r)
}
