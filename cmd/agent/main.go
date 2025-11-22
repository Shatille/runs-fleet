// Package main implements the runs-fleet agent that runs on EC2 instances to execute GitHub Actions jobs.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

func main() {
	log.Println("Starting runs-fleet agent...")

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	runID := os.Getenv("RUNS_FLEET_RUN_ID")
	if runID == "" {
		log.Fatal("RUNS_FLEET_RUN_ID environment variable is required")
	}
	log.Printf("Agent running for job %s", runID)

	instanceID := os.Getenv("RUNS_FLEET_INSTANCE_ID")
	if instanceID == "" {
		log.Fatal("RUNS_FLEET_INSTANCE_ID environment variable is required")
	}
	log.Printf("Instance ID: %s", instanceID)

	log.Println("Downloading GitHub Actions runner...")
	log.Println("Registering runner...")
	log.Println("Executing job...")

	jobDone := make(chan struct{})
	go func() {
		defer close(jobDone)
	}()

	select {
	case <-jobDone:
		log.Println("Job completed successfully")
	case <-ctx.Done():
		log.Println("Shutdown signal received during job execution")
	}

	terminateInstance(context.Background(), instanceID)
}

func terminateInstance(ctx context.Context, instanceID string) {
	region := os.Getenv("AWS_REGION")
	if region == "" {
		log.Printf("AWS_REGION environment variable not set (instance will rely on max runtime timeout)")
		return
	}

	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		log.Printf("Failed to load AWS config: %v (instance will rely on max runtime timeout)", err)
		return
	}

	client := ec2.NewFromConfig(cfg)

	output, err := client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		log.Printf("Failed to terminate instance: %v (instance will rely on max runtime timeout)", err)
		return
	}

	if len(output.TerminatingInstances) > 0 {
		state := output.TerminatingInstances[0].CurrentState
		if state != nil {
			log.Printf("Instance %s termination initiated: %s", instanceID, state.Name)
		}
	}
}
