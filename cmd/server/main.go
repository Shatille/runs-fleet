// Package main implements the runs-fleet orchestrator server that processes GitHub webhook events.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/cache"
	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/events"
	"github.com/Shavakan/runs-fleet/pkg/fleet"
	gh "github.com/Shavakan/runs-fleet/pkg/github"
	"github.com/Shavakan/runs-fleet/pkg/metrics"
	"github.com/Shavakan/runs-fleet/pkg/pools"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/go-github/v57/github"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log.Println("Starting runs-fleet server...")

	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Initialize AWS clients
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.AWSRegion))
	if err != nil {
		log.Fatalf("Failed to load AWS config: %v", err)
	}

	// Initialize components
	sqsClient := queue.NewClient(awsCfg, cfg.QueueURL)
	eventsQueueClient := queue.NewClient(awsCfg, cfg.EventsQueueURL)
	fleetManager := fleet.NewManager(awsCfg, cfg)
	dbClient := db.NewClient(awsCfg, cfg.PoolsTableName)
	poolManager := pools.NewManager(dbClient, fleetManager, cfg)
	cacheServer := cache.NewServer(awsCfg, cfg.CacheBucketName)
	metricsPublisher := metrics.NewPublisher(awsCfg)
	eventHandler := events.NewHandler(eventsQueueClient, dbClient, metricsPublisher, cfg)

	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "OK\n")
	})

	cacheHandler := cache.NewHandler(cacheServer)
	cacheHandler.RegisterRoutes(mux)

	mux.HandleFunc("/webhook", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		payload, err := gh.ParseWebhook(r, cfg.GitHubWebhookSecret)
		if err != nil {
			log.Printf("Webhook parsing failed: %v", err)
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}

		switch event := payload.(type) {
		case *github.WorkflowJobEvent:
			if event.GetAction() == "queued" {
				handleWorkflowJob(ctx, event, sqsClient, metricsPublisher)
			}
		}

		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "Job queued\n")
	})

	server := &http.Server{
		Addr:         ":8080",
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start worker in background
	go runWorker(ctx, sqsClient, fleetManager, poolManager, metricsPublisher, cfg)

	// Start pool reconciliation loop
	go poolManager.ReconcileLoop(ctx)

	// Start event handler loop
	go eventHandler.Run(ctx)

	go func() {
		log.Printf("Server listening on %s", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("Shutdown signal received, gracefully stopping...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("Server shutdown failed: %v", err)
	}

	log.Println("Server stopped")
}

func handleWorkflowJob(ctx context.Context, event *github.WorkflowJobEvent, q *queue.Client, m *metrics.Publisher) {
	log.Printf("Received workflow_job queued: %s", event.GetWorkflowJob().GetName())

	jobConfig, err := gh.ParseLabels(event.GetWorkflowJob().Labels)
	if err != nil {
		log.Printf("Skipping job (no runs-fleet labels): %v", err)
		return
	}

	msg := &queue.JobMessage{
		RunID:        jobConfig.RunID,
		InstanceType: jobConfig.InstanceType,
		Pool:         jobConfig.Pool,
		Private:      jobConfig.Private,
		Spot:         jobConfig.Spot,
		RunnerSpec:   jobConfig.RunnerSpec,
	}

	if err := q.SendMessage(ctx, msg); err != nil {
		log.Printf("Failed to enqueue job: %v", err)
		return
	}

	m.PublishQueueDepth(ctx, 1) // Increment queue depth (approximate)
	log.Printf("Enqueued job for run %s", jobConfig.RunID)
}

func runWorker(ctx context.Context, q *queue.Client, f *fleet.FleetManager, pm *pools.Manager, m *metrics.Publisher, cfg *config.Config) {
	log.Println("Starting worker loop...")
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			messages, err := q.ReceiveMessages(ctx, 10, 20) // Long polling
			if err != nil {
				log.Printf("Failed to receive messages: %v", err)
				continue
			}

			for _, msg := range messages {
				processMessage(ctx, q, f, pm, m, msg, cfg)
			}
		}
	}
}

func processMessage(ctx context.Context, q *queue.Client, f *fleet.FleetManager, pm *pools.Manager, m *metrics.Publisher, msg types.Message, cfg *config.Config) {
	startTime := time.Now()
	defer func() {
		m.PublishJobDuration(ctx, time.Since(startTime).Seconds())
	}()

	var job queue.JobMessage
	if err := json.Unmarshal([]byte(*msg.Body), &job); err != nil {
		log.Printf("Failed to unmarshal message: %v", err)
		// Delete poison message
		_ = q.DeleteMessage(ctx, *msg.ReceiptHandle)
		return
	}

	log.Printf("Processing job for run %s", job.RunID)

	// Try to get from pool if requested
	if job.Pool != "" {
		instanceID, err := pm.GetInstance(ctx, job.Pool)
		if err == nil && instanceID != "" {
			log.Printf("Claimed instance %s from pool %s for run %s", instanceID, job.Pool, job.RunID)
			// TODO: Assign job to instance (SSM/GitHub registration)
			if err := q.DeleteMessage(ctx, *msg.ReceiptHandle); err != nil {
				log.Printf("Failed to delete message: %v", err)
			}
			m.PublishQueueDepth(ctx, -1) // Decrement queue depth
			return
		}
		log.Printf("Pool %s empty or unavailable, falling back to cold start", job.Pool)
	}

	// Determine subnet
	subnetID := ""
	if len(cfg.PublicSubnetIDs) > 0 {
		subnetID = cfg.PublicSubnetIDs[0] // Simple round-robin or random could be better
	}

	spec := &fleet.LaunchSpec{
		RunID:        job.RunID,
		InstanceType: job.InstanceType,
		SubnetID:     subnetID,
		Spot:         job.Spot,
	}

	if err := f.CreateFleet(ctx, spec); err != nil {
		log.Printf("Failed to create fleet: %v", err)
		// Don't delete message so it retries (visibility timeout)
		return
	}

	log.Printf("Successfully launched instance for run %s", job.RunID)
	if err := q.DeleteMessage(ctx, *msg.ReceiptHandle); err != nil {
		log.Printf("Failed to delete message: %v", err)
	}
	m.PublishQueueDepth(ctx, -1) // Decrement queue depth
	m.PublishFleetSize(ctx, 1)   // Increment fleet size
}
