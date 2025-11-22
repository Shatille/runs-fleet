package main

import (
	"context"
	"log"
	"os"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

func main() {
	log.Println("Starting runs-fleet agent...")

	// In a real scenario, we would fetch config from SSM or User Data
	// For MVP, we assume env vars are set by User Data script
	runID := os.Getenv("RUNS_FLEET_RUN_ID")
	instanceID := os.Getenv("RUNS_FLEET_INSTANCE_ID")

	if runID == "" {
		log.Println("Warning: RUNS_FLEET_RUN_ID not set")
	} else {
		log.Printf("Agent running for job %s", runID)
	}

	if instanceID == "" {
		log.Println("Warning: RUNS_FLEET_INSTANCE_ID not set")
	} else {
		log.Printf("Instance ID: %s", instanceID)
	}

	// 1. Download Runner (Mock)
	log.Println("Downloading GitHub Actions runner...")

	// 2. Register Runner (Mock)
	log.Println("Registering runner...")

	// 3. Run Job
	log.Println("Waiting for job...")

	// 4. Self-terminate
	if instanceID != "" {
		log.Println("Job complete, terminating instance...")
		terminateInstance(instanceID)
	} else {
		log.Println("No instance ID, skipping termination (local dev?)")
	}
}

func terminateInstance(instanceID string) {
	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		log.Printf("Failed to load AWS config: %v", err)
		return
	}

	client := ec2.NewFromConfig(cfg)

	_, err = client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		log.Printf("Failed to terminate instance: %v", err)
	}
}
