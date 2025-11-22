// Package main implements the runs-fleet orchestrator server that processes GitHub webhook events.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/github"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	gh "github.com/google/go-github/v57/github"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log.Println("Starting runs-fleet server...")

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.AWSRegion))
	if err != nil {
		log.Fatalf("Failed to load AWS config: %v", err)
	}

	queueClient := queue.NewClient(awsCfg, cfg.QueueURL)

	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "OK\n")
	})

	mux.HandleFunc("/webhook", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		payload, err := github.ParseWebhook(r, cfg.GitHubWebhookSecret)
		if err != nil {
			log.Printf("Webhook validation failed: %v", err)
			http.Error(w, "Webhook validation failed", http.StatusUnauthorized)
			return
		}

		event, ok := payload.(*gh.WorkflowJobEvent)
		if !ok || event.Action == nil || *event.Action != "queued" {
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, "Event ignored\n")
			return
		}

		if event.WorkflowJob == nil || event.WorkflowJob.Labels == nil {
			log.Printf("Workflow job missing labels")
			w.WriteHeader(http.StatusOK)
			return
		}

		if event.WorkflowJob.ID == nil {
			log.Printf("Workflow job missing ID")
			w.WriteHeader(http.StatusOK)
			return
		}

		jobConfig, err := github.ParseLabels(event.WorkflowJob.Labels)
		if err != nil {
			log.Printf("Failed to parse labels: %v", err)
			w.WriteHeader(http.StatusOK)
			return
		}

		jobID := strconv.FormatInt(*event.WorkflowJob.ID, 10)

		job := &queue.JobMessage{
			JobID:        jobID,
			RunID:        jobConfig.RunID,
			InstanceType: jobConfig.InstanceType,
			Pool:         jobConfig.Pool,
			Private:      jobConfig.Private,
			Spot:         jobConfig.Spot,
			RunnerSpec:   jobConfig.RunnerSpec,
		}

		if err := queueClient.SendMessage(r.Context(), job); err != nil {
			log.Printf("Failed to send message to queue: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
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
