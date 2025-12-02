package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/gin-gonic/gin"
)

// SleepWorkflowInput defines the input for the sleep workflow
type SleepWorkflowInput struct {
	DurationSeconds int `json:"duration_seconds"`
}

// MetricsResponse represents the response from the /metrics endpoint
type MetricsResponse struct {
	ExpectedPods int `json:"expected_pods"`
}

// WorkflowQueueMetadata represents the queue metadata from the admin endpoint
type WorkflowQueueMetadata struct {
	WorkerConcurrency int `json:"worker_concurrency"`
}

// SleepWorkflow sleeps for the configured duration
func SleepWorkflow(ctx dbos.DBOSContext, input SleepWorkflowInput) (string, error) {
	duration := time.Duration(input.DurationSeconds) * time.Second
	dbos.Sleep(ctx, duration)
	return fmt.Sprintf("Slept for %d seconds", input.DurationSeconds), nil
}

func main() {
	dbosContext, err := dbos.NewDBOSContext(context.Background(), dbos.Config{
		AppName:     "dbos-starter",
		DatabaseURL: os.Getenv("DBOS_SYSTEM_DATABASE_URL"),
		AdminServer: true, // Enable admin server to access queue metadata
	})
	if err != nil {
		panic(fmt.Sprintf("Initializing DBOS failed: %v", err))
	}

	// Create queue with worker concurrency = 10
	queue := dbos.NewWorkflowQueue(dbosContext, "queue1", dbos.WithWorkerConcurrency(1))

	// Register the sleep workflow
	dbos.RegisterWorkflow(dbosContext, SleepWorkflow)

	err = dbos.Launch(dbosContext)
	if err != nil {
		panic(fmt.Sprintf("Launching DBOS failed: %v", err))
	}
	defer dbos.Shutdown(dbosContext, 5*time.Second)

	r := gin.Default()

	// Metrics endpoint for KEDA autoscaling
	r.GET("/metrics", func(c *gin.Context) {
		expectedPods, err := computeExpectedPods(dbosContext)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Error computing metrics: %v", err)})
			return
		}

		c.JSON(http.StatusOK, MetricsResponse{ExpectedPods: expectedPods})
	})

	// Handler to enqueue a workflow with configurable sleep duration
	r.GET("/enqueue/:duration", func(c *gin.Context) {
		// Get duration from URL path parameter
		durationStr := c.Param("duration")
		duration, err := strconv.Atoi(durationStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Invalid duration: %v", err)})
			return
		}

		input := SleepWorkflowInput{
			DurationSeconds: duration,
		}

		handle, err := dbos.RunWorkflow(dbosContext, SleepWorkflow, input, dbos.WithQueue(queue.Name))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Error enqueuing workflow: %v", err)})
			return
		}

		workflowID := handle.GetWorkflowID()

		c.JSON(http.StatusOK, gin.H{
			"message":     "Workflow enqueued successfully",
			"workflow_id": workflowID,
			"duration":    input.DurationSeconds,
		})
	})

	r.Run(":8000")
}

// computeExpectedPods computes the maximum expected pods needed across all queues with worker concurrency
func computeExpectedPods(ctx dbos.DBOSContext) (int, error) {
	// Get queue metadata from admin server
	adminPort := 3001 // Default admin server port
	queueMetadataURL := fmt.Sprintf("http://localhost:%d/dbos-workflow-queues-metadata", adminPort)

	resp, err := http.Get(queueMetadataURL)
	if err != nil {
		return 0, fmt.Errorf("failed to fetch queue metadata: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("admin endpoint returned status %d: %s", resp.StatusCode, string(body))
	}

	// Parse the response: array of objects with queue name and metadata
	var queueMetadataArray []struct {
		Name              string `json:"name"`
		WorkerConcurrency int    `json:"workerConcurrency"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&queueMetadataArray); err != nil {
		return 0, fmt.Errorf("failed to decode queue metadata: %w", err)
	}

	// Filter queues that have worker concurrency > 0
	queuesWithConcurrency := make(map[string]int)
	for _, queue := range queueMetadataArray {
		if queue.WorkerConcurrency > 0 {
			queuesWithConcurrency[queue.Name] = queue.WorkerConcurrency
		}
	}

	if len(queuesWithConcurrency) == 0 {
		return 1, nil // Default to 1 pod if no queues with concurrency
	}

	// Get all workflows that are in enqueued/pending in any queue
	allWorkflows, err := dbos.ListWorkflows(ctx, dbos.WithQueuesOnly())
	if err != nil {
		return 0, fmt.Errorf("failed to list workflows: %w", err)
	}

	// Build a map of queue name to workflow count in a single pass
	queueWorkflowCounts := make(map[string]int)
	for _, workflow := range allWorkflows {
		queueWorkflowCounts[workflow.QueueName]++
	}

	// Compute max expected pods across all queues with worker concurrency
	maxExpectedPods := 0
	for queueName, workerConcurrency := range queuesWithConcurrency {
		queueLength := queueWorkflowCounts[queueName]
		// Compute expected pods for this queue: ceil((enqueued + pending) / worker_concurrency)
		expectedPods := int(math.Ceil(float64(queueLength) / float64(workerConcurrency)))
		if expectedPods > maxExpectedPods {
			maxExpectedPods = expectedPods
		}
	}

	// Ensure at least 1 pod
	if maxExpectedPods < 1 {
		maxExpectedPods = 1
	}

	return maxExpectedPods, nil
}
