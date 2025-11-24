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
	"sync"
	"sync/atomic"
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

		processed := false
		switch event := payload.(type) {
		case *github.WorkflowJobEvent:
			if event.GetAction() == "queued" {
				handleWorkflowJob(r.Context(), event, sqsClient, metricsPublisher)
				processed = true
			}
		}

		w.WriteHeader(http.StatusOK)
		if processed {
			_, _ = fmt.Fprintf(w, "Job queued\n")
		} else {
			_, _ = fmt.Fprintf(w, "Event acknowledged\n")
		}
	})

	server := &http.Server{
		Addr:         ":8080",
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start worker in background
	var subnetIndex uint64
	go runWorker(ctx, sqsClient, fleetManager, poolManager, metricsPublisher, cfg, &subnetIndex)

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
		JobID:        fmt.Sprintf("%d", event.GetWorkflowJob().GetID()),
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

	if err := m.PublishQueueDepth(ctx, 1); err != nil {
		log.Printf("Failed to publish queue depth metric: %v", err)
	}
	log.Printf("Enqueued job for run %s", jobConfig.RunID)
}

func runWorker(ctx context.Context, q *queue.Client, f *fleet.Manager, pm *pools.Manager, m *metrics.Publisher, cfg *config.Config, subnetIndex *uint64) {
	log.Println("Starting worker loop...")
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	const maxConcurrency = 5
	sem := make(chan struct{}, maxConcurrency)
	var activeWork sync.WaitGroup

	defer func() {
		log.Println("Waiting for in-flight work to complete...")
		activeWork.Wait()
		log.Println("Worker shutdown complete")
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			timeout := 25 * time.Second
			if deadline, ok := ctx.Deadline(); ok {
				remaining := time.Until(deadline)
				if remaining < timeout {
					timeout = remaining
				}
			}
			recvCtx, cancel := context.WithTimeout(ctx, timeout)
			messages, err := q.ReceiveMessages(recvCtx, 10, 20)
			cancel()
			if err != nil {
				log.Printf("Failed to receive messages: %v", err)
				continue
			}

			if len(messages) == 0 {
				continue
			}

			for _, msg := range messages {
				msg := msg
				activeWork.Add(1)
				go func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("panic in processMessage: %v", r)
						}
					}()
					defer activeWork.Done()
					sem <- struct{}{}
					defer func() { <-sem }()

					processCtx, processCancel := context.WithTimeout(ctx, 30*time.Second)
					defer processCancel()
					processMessage(processCtx, q, f, pm, m, msg, cfg, subnetIndex)
				}()
			}
		}
	}
}

func processMessage(ctx context.Context, q *queue.Client, f *fleet.Manager, _ *pools.Manager, m *metrics.Publisher, msg types.Message, cfg *config.Config, subnetIndex *uint64) {
	startTime := time.Now()
	defer func() {
		if err := m.PublishJobDuration(ctx, time.Since(startTime).Seconds()); err != nil {
			log.Printf("Failed to publish job duration metric: %v", err)
		}
	}()

	var job queue.JobMessage
	if err := json.Unmarshal([]byte(*msg.Body), &job); err != nil {
		log.Printf("Failed to unmarshal message: %v", err)
		_ = q.DeleteMessage(ctx, *msg.ReceiptHandle)
		metricCtx, metricCancel := context.WithTimeout(ctx, 3*time.Second)
		defer metricCancel()
		if metricErr := m.PublishMessageDeletionFailure(metricCtx); metricErr != nil {
			log.Printf("Failed to publish poison message metric: %v", metricErr)
		}
		return
	}

	log.Printf("Processing job for run %s", job.RunID)

	subnetID := ""
	if job.Private && len(cfg.PrivateSubnetIDs) > 0 {
		idx := atomic.AddUint64(subnetIndex, 1) - 1
		subnetID = cfg.PrivateSubnetIDs[idx%uint64(len(cfg.PrivateSubnetIDs))]
	} else if len(cfg.PublicSubnetIDs) > 0 {
		idx := atomic.AddUint64(subnetIndex, 1) - 1
		subnetID = cfg.PublicSubnetIDs[idx%uint64(len(cfg.PublicSubnetIDs))]
	}

	spec := &fleet.LaunchSpec{
		RunID:        job.RunID,
		InstanceType: job.InstanceType,
		SubnetID:     subnetID,
		Spot:         job.Spot,
	}

	instanceIDs, err := f.CreateFleet(ctx, spec)
	if err != nil {
		log.Printf("Failed to create fleet: %v", err)
		return
	}

	log.Printf("Successfully launched %d instance(s) for run %s", len(instanceIDs), job.RunID)
	if err := q.DeleteMessage(ctx, *msg.ReceiptHandle); err != nil {
		log.Printf("Failed to delete message: %v", err)
	}
	if err := m.PublishQueueDepth(ctx, -1); err != nil {
		log.Printf("Failed to publish queue depth metric: %v", err)
	}
	metricCtx, metricCancel := context.WithTimeout(ctx, 3*time.Second)
	defer metricCancel()
	if metricErr := m.PublishFleetSizeIncrement(metricCtx); metricErr != nil {
		log.Printf("Failed to publish fleet size increment metric: %v", metricErr)
	}
}
