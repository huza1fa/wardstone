package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/wardstone-project/wardstone/internal/admin"
	"github.com/wardstone-project/wardstone/internal/agent"
	"github.com/wardstone-project/wardstone/internal/api"
	"github.com/wardstone-project/wardstone/internal/approvals"
	"github.com/wardstone-project/wardstone/internal/capabilities"
	"github.com/wardstone-project/wardstone/internal/config"
	"github.com/wardstone-project/wardstone/internal/connectors"
	"github.com/wardstone-project/wardstone/internal/connectors/google"
	"github.com/wardstone-project/wardstone/internal/connectors/jira"
	"github.com/wardstone-project/wardstone/internal/database"
	"github.com/wardstone-project/wardstone/internal/delivery"
	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/investigations"
	"github.com/wardstone-project/wardstone/internal/models"
	"github.com/wardstone-project/wardstone/internal/policy"
	"github.com/wardstone-project/wardstone/internal/readiness"
	"github.com/wardstone-project/wardstone/internal/specialists"
	"github.com/wardstone-project/wardstone/internal/worker"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("wardstone stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	settings, err := config.Load()
	if err != nil {
		return err
	}
	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := database.Open(rootCtx, settings.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	store := database.NewStore(pool)

	registry := capabilities.NewRegistry()
	if err := registry.Register(google.Capabilities()...); err != nil {
		return err
	}
	policyEvaluator := policy.New(settings.Mode, registry, settings.PolicyRules)

	httpClient := &http.Client{Transport: &http.Transport{
		DialContext:  (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns: 100, MaxIdleConnsPerHost: 10, IdleConnTimeout: 90 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: settings.ModelTimeout,
	}}
	var googleClient *google.HTTPClient
	collectors := make([]connectors.EvidenceCollector, 0, 2)
	if settings.GoogleConfigured() {
		googleClient, err = google.NewHTTPClient(settings.GoogleBaseURL, settings.GoogleAccessToken, httpClient)
		if err != nil {
			return err
		}
		collectors = append(collectors, google.UserCollector{Client: googleClient}, google.GroupCollector{Client: googleClient})
	}
	var model *models.OpenAICompatible
	if settings.ModelConfigured() {
		model, err = models.NewOpenAICompatible(settings.OpenAIBaseURL, settings.OpenAIAPIKey, settings.OpenAIModel, httpClient)
		if err != nil {
			return err
		}
	}
	var jiraClient *jira.Client
	if settings.JiraConfigured() {
		jiraClient, err = jira.NewClient(settings.JiraBaseURL, settings.JiraEmail, settings.JiraAPIToken, httpClient)
		if err != nil {
			return err
		}
	}
	specialistRegistry, err := specialists.NewRegistry(settings.Specialists...)
	if err != nil {
		return err
	}
	modelProvider := agent.ModelProvider(routingOnlyModel{})
	if model != nil {
		modelProvider = model
	}
	service, err := investigations.NewService(store, collectors, modelProvider, registry, policyEvaluator, investigations.Config{
		MaxConcurrentCollectors:  settings.MaxCollectors,
		CollectorTimeout:         settings.CollectorTimeout,
		ModelTimeout:             settings.ModelTimeout,
		PromptVersion:            "shadow-v1",
		MaxHandoffs:              settings.MaxHandoffs,
		MaxFollowUpQuestions:     settings.MaxFollowUpQuestions,
		DeliverRequesterMessages: settings.Mode != domain.OperatingModeShadow,
		Specialists:              specialistRegistry,
		RoutingRules:             settings.RoutingRules,
		IntentClassification:     &settings.IntentClassification,
	})
	if err != nil {
		return err
	}
	var workerPool *worker.Pool
	if model != nil {
		workerPool, err = worker.NewPool(store, service, logger, settings.Workers, settings.JobLease, 500*time.Millisecond)
		if err != nil {
			return err
		}
	}
	var deliveryDispatcher *delivery.Dispatcher
	if jiraClient != nil && settings.Mode != domain.OperatingModeShadow {
		deliveryDispatcher, err = delivery.NewDispatcher(store, jiraClient, logger, delivery.Config{
			Lease: settings.JobLease, PollInterval: 500 * time.Millisecond, MaxAttempts: 5,
		})
		if err != nil {
			return err
		}
	}
	approvalService, err := approvals.NewService(store, settings.ApprovalLifetime)
	if err != nil {
		return err
	}
	setupService, err := readiness.New(setupChecks(settings, pool, jiraClient, googleClient, model), admin.RuntimeFeatures{
		JiraIntake: settings.JiraInboundConfigured() && model != nil, Investigations: model != nil,
		GoogleEvidence: googleClient != nil && model != nil, JiraDelivery: deliveryDispatcher != nil,
	}, 5*time.Second, logger)
	if err != nil {
		return err
	}
	options := []api.Option{api.WithAdmin(store, approvalService, settings.Mode), api.WithSetup(setupService), api.WithRoutingPreview(service)}
	var intake api.InvestigationService
	if model != nil {
		intake = service
		options = append(options, api.WithConversations(service))
	}
	apiServer, err := api.NewServer(intake, store, settings.JiraWebhookSecret, settings.OperatorToken, options...)
	if err != nil {
		return err
	}
	httpServer := &http.Server{
		Addr: settings.ListenAddress, Handler: apiServer.Handler(),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second,
	}

	group, ctx := errgroup.WithContext(rootCtx)
	group.Go(func() error {
		logger.Info("HTTP server listening", "address", settings.ListenAddress, "mode", settings.Mode)
		err := httpServer.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	})
	if workerPool != nil {
		group.Go(func() error {
			err := workerPool.Run(ctx)
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		})
	}
	if deliveryDispatcher != nil {
		group.Go(func() error {
			err := deliveryDispatcher.Run(ctx)
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		})
	}
	group.Go(func() error {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	})
	return group.Wait()
}

// routingOnlyModel allows deterministic routing preview while workers and Jira
// intake remain disabled. Diagnose is unreachable because no worker is started.
type routingOnlyModel struct{}

func (routingOnlyModel) Name() domain.ModelProviderName { return "unconfigured" }
func (routingOnlyModel) Model() string                  { return "unconfigured" }
func (routingOnlyModel) Diagnose(context.Context, agent.Request) (agent.Result, error) {
	return agent.Result{}, errors.New("model provider is not configured")
}

func setupChecks(settings config.Config, pool *pgxpool.Pool, jiraClient *jira.Client, googleClient *google.HTTPClient, model *models.OpenAICompatible) []readiness.Check {
	databaseProbe := func(ctx context.Context) error {
		err := database.CheckSchema(ctx, pool)
		if database.IsSchemaMismatch(err) {
			return connectors.NewProbeError(connectors.ProbeSchemaMismatch)
		}
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			return connectors.NewProbeError(connectors.ProbeUnavailable)
		}
		return nil
	}
	checks := []readiness.Check{
		{Name: "database", Label: "PostgreSQL", Description: "Durable control-plane storage and schema.", Permission: "Connect and read/write Wardstone runtime tables.", Required: true, Configured: true, Probe: databaseProbe, Ready: true, ReadyCode: "ready", ReadyMessage: "Connected with the expected schema."},
		{Name: "policy", Label: "Policy and specialists", Description: "Administrator-owned capability, specialist, and routing configuration.", Permission: "Read the configured YAML file at startup.", Required: true, Configured: true, Ready: true, ReadyCode: "loaded", ReadyMessage: "Configuration loaded and validated."},
		{Name: "jira_inbound", Label: "Jira intake", Description: "Authenticated ticket and requester-reply webhooks.", Permission: "A distinct shared webhook secret; Jira Automation is configured separately.", Required: true, Configured: settings.JiraInboundConfigured(), Ready: settings.JiraInboundConfigured(), ReadyCode: "configured", ReadyMessage: "Webhook authentication is configured; delivery from Jira cannot be tested from Wardstone."},
		{Name: "jira", Label: "Jira API", Description: "Readiness for reviewed requester-message delivery.", Permission: "Dedicated Jira account; Add comments is not used in SHADOW mode.", Configured: jiraClient != nil},
		{Name: "google", Label: "Google Workspace", Description: "Read-only user and group evidence collection.", Permission: "Directory user and group read scopes.", Configured: googleClient != nil},
		{Name: "model", Label: "Model provider", Description: "OpenAI-compatible diagnosis and intent classification.", Permission: "Read configured model metadata; no ticket content is sent by this test.", Required: true, Configured: model != nil},
	}
	if jiraClient != nil {
		checks[3].Probe = jiraClient.Probe
	}
	if googleClient != nil {
		checks[4].Probe = googleClient.Probe
	}
	if model != nil {
		checks[5].Probe = model.Probe
	}
	return checks
}
