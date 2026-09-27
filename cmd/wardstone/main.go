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

	"golang.org/x/sync/errgroup"

	"github.com/wardstone-project/wardstone/internal/api"
	"github.com/wardstone-project/wardstone/internal/approvals"
	"github.com/wardstone-project/wardstone/internal/capabilities"
	"github.com/wardstone-project/wardstone/internal/config"
	"github.com/wardstone-project/wardstone/internal/connectors"
	"github.com/wardstone-project/wardstone/internal/connectors/google"
	"github.com/wardstone-project/wardstone/internal/connectors/jira"
	"github.com/wardstone-project/wardstone/internal/database"
	"github.com/wardstone-project/wardstone/internal/delivery"
	"github.com/wardstone-project/wardstone/internal/investigations"
	"github.com/wardstone-project/wardstone/internal/models"
	"github.com/wardstone-project/wardstone/internal/policy"
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
	googleClient, err := google.NewHTTPClient(settings.GoogleBaseURL, settings.GoogleAccessToken, httpClient)
	if err != nil {
		return err
	}
	model, err := models.NewOpenAICompatible(settings.OpenAIBaseURL, settings.OpenAIAPIKey, settings.OpenAIModel, httpClient)
	if err != nil {
		return err
	}
	jiraClient, err := jira.NewClient(settings.JiraBaseURL, settings.JiraEmail, settings.JiraAPIToken, httpClient)
	if err != nil {
		return err
	}
	deliveryDispatcher, err := delivery.NewDispatcher(store, jiraClient, logger, delivery.Config{
		Lease: settings.JobLease, PollInterval: 500 * time.Millisecond, MaxAttempts: 5,
	})
	if err != nil {
		return err
	}
	collectors := []connectors.EvidenceCollector{
		google.UserCollector{Client: googleClient},
		google.GroupCollector{Client: googleClient},
	}
	service, err := investigations.NewService(store, collectors, model, registry, policyEvaluator, investigations.Config{
		MaxConcurrentCollectors: settings.MaxCollectors,
		CollectorTimeout:        settings.CollectorTimeout,
		ModelTimeout:            settings.ModelTimeout,
		PromptVersion:           "shadow-v1",
	})
	if err != nil {
		return err
	}
	workerPool, err := worker.NewPool(store, service, logger, settings.Workers, settings.JobLease, 500*time.Millisecond)
	if err != nil {
		return err
	}
	approvalService, err := approvals.NewService(store, settings.ApprovalLifetime)
	if err != nil {
		return err
	}
	apiServer, err := api.NewServer(service, store, settings.JiraWebhookSecret, settings.OperatorToken,
		api.WithAdmin(store, approvalService, settings.Mode), api.WithConversations(service))
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
	group.Go(func() error {
		err := workerPool.Run(ctx)
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	})
	group.Go(func() error {
		err := deliveryDispatcher.Run(ctx)
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	})
	group.Go(func() error {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	})
	return group.Wait()
}
