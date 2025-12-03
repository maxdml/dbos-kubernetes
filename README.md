# Autoscale a DBOS application on Kubernetes with KEDA

Queues are the prime mechanism to control load in a DBOS application. For example you can set a per-worker concurrency cap on a DBOS queue, controlling how many tasks a single worker can dequeue. You can then estimate how many workers are required at any given time to handle a queue's tasks by dividing the number of tasks in the queue by the worker concurrency limit.

In this tutorial, we walk you through configuring KEDA on Kubernetes to scale pods based on DBOS queue utilization, using the metric API.

## The application

For this tutorial we'll use a single queue with worker concurrency limits. The application will expose an endpoint which will enqueue a single workflow, set to sleep for a configurable duration.

The code snippets will be in Golang but the concept works across all DBOS SDKs. This repository runs the example and has utility scripts assuming as much.

<details><summary><strong>Configuration</strong></summary>

Before deploying, you need to configure environment-specific values:

1. Copy the example environment file:
   ```bash
   cp .env.sh.example .env.sh
   ```

2. Edit `.env.sh` and update the following values:
   - **ECR_REPO**: Your AWS ECR repository URL (e.g., `ACCOUNT_ID.dkr.ecr.REGION.amazonaws.com/REPO_NAME`)
   - **AWS_REGION**: Your AWS region (e.g., `us-east-1`)
   - **AWS_LB_SUBNETS**: Comma-separated list of subnet IDs for your AWS Load Balancer
   - **POSTGRES_PASSWORD**: A secure password for PostgreSQL
   - **KUBERNETES_NAMESPACE**: The Kubernetes namespace where you'll deploy (default: `default`)
   - **DBOS_APP_NAME**: The name for your DBOS application (default: `dbos-app`)

   The `.env.sh` file is gitignored and will not be committed to the repository.

3. Generate Kubernetes manifests from templates:
   ```bash
   ./deploy.sh
   ```

   This will create generated manifests in `manifests/generated/` directory.

4. To deploy to your cluster:
   ```bash
   ./deploy.sh --apply
   ```

   Or apply manually:
   ```bash
   kubectl apply -f manifests/generated/
   ```

</details>

## Setup

This tutorial assume you already have a Kubernetes cluster deployed. You'll need a Postgres instance to backup your application.
<details><summary><strong>Sample Postgres manifest</strong></summary>

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: postgres
spec:
  replicas: 1
  selector:
    matchLabels:
      app: postgres
  template:
    metadata:
      labels:
        app: postgres
    spec:
      containers:
        - name: postgres
          image: pgvector/pgvector:pg16
          env:
            - name: POSTGRES_USER
              value: "postgres"
            - name: POSTGRES_PASSWORD
              value: "dbos"
          ports:
            - containerPort: 5432
          volumeMounts:
            - mountPath: /var/lib/postgresql/data
              name: postgres-storage
      volumes:
        - name: postgres-storage
          emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: postgres
spec:
  selector:
    app: postgres
  ports:
    - port: 5432
      targetPort: 5432
```

</details>

### Install KEDA

[Install KEDA](https://keda.sh/docs/2.18/deploy/). To verify KEDA is running:

```bash
kubectl get pods -n keda
```

You should see KEDA operator and metrics server pods running.

### Build and Push Docker Image

Build and push your Docker image to ECR:

```bash
./build-and-push.sh [tag]
```

If no tag is provided, it will use the `IMAGE_TAG` value from `.env.sh` (default: `latest`).

### Deploy a DBOS application

The deployment manifests are generated from templates using the values in your `.env.sh` file. See the [Configuration](#configuration) section above for setup instructions.

<details><summary><strong>Sample DBOS application manifest</strong></summary>

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: dbos-app
spec:
  replicas: 1
  selector:
    matchLabels:
      app: dbos-app
  template:
    metadata:
      labels:
        app: dbos-app
    spec:
      containers:
        - name: dbos-app
          image: YOUR_ECR_REPO:kubernetes-integration-latest
          env:
            - name: DBOS_SYSTEM_DATABASE_URL
              value: postgres://postgres:YOUR_PASSWORD@postgres:5432/kube
          ports:
            - containerPort: 8000
---
apiVersion: v1
kind: Service
metadata:
  name: dbos-app
  annotations:
    service.beta.kubernetes.io/aws-load-balancer-scheme: "internet-facing"
    service.beta.kubernetes.io/aws-load-balancer-subnets: "subnet-xxx,subnet-yyy,subnet-zzz"
spec:
  type: LoadBalancer
  selector:
    app: dbos-app
  ports:
    - port: 8000
      targetPort: 8000
```

</details>

### Configure a KEDA scaled object

Now we will instruct KEDA to scale our application's pods based on a queue utilization metric exposed by the application itself. The KEDA ScaledObject manifest is generated from templates and will automatically use your configured namespace and app name.

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: dbos-app-scaledobject
spec:
  scaleTargetRef:
    name: dbos-app
  minReplicaCount: 1
  maxReplicaCount: 100  # Adjust as needed
  triggers:
  - type: metrics-api
    metadata:
      url: http://dbos-app.default.svc.cluster.local:8000/metrics/queueName
      valueLocation: queue_length
      targetValue: "2"  # Set to your worker concurrency value
```

The `valueLocation` field represents a JSON field in the /metrics endpoint response, where we expect the metric's value to reside. `targetValue: "2"` means we want to scale when the queue length exceeds 2 times the worker concurrency (in this example, worker concurrency is 2, so we scale when queue length > 4).

## The metrics endpoint

The endpoints we registered with the KEDA scaler returns the number of workers needed to handle the busiest queue's load. You can of course change this logic for any metric of your choice (e.g., target a specific queue, or sum across all queues.)

```golang
type MetricsResponse struct {
	ExpectedPods int `json:"expected_pods"`
}

r.GET("/metrics", func(c *gin.Context) {
    expectedPods, err := computeExpectedPods(dbosContext)
    if err != nil {
        c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Error computing metrics: %v", err)})
        return
    }
    c.JSON(http.StatusOK, MetricsResponse{ExpectedPods: expectedPods})
})

```
### Finding the busiest queue's scaling factor

```golang
func computeExpectedWorkers(ctx dbos.DBOSContext) (int, error) {
	// Get queue metadata from admin server
	// Filter queues that have worker concurrency > 0
	queuesWithConcurrency := make(map[string]int)
	for _, queue := range ctx.ListRegisteredQueues {
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
```

## Try it

First, get your Load Balancer URL:

```bash
kubectl get service dbos-app -o jsonpath='{.status.loadBalancer.ingress[0].hostname}'
```

Then, using the endpoint in the application, enqueue a number of workflows that exceeds the concurrency limit, for example:

```bash
# Replace YOUR_LOAD_BALANCER with the hostname from above
# Enqueue 10 workflows that sleep for 30 seconds each
for i in {1..10}; do curl -s http://YOUR_LOAD_BALANCER:8000/enqueue/30 & done;
```

Watch the pods scale up:

```bash
# Watch pods in real-time
watch -n 1 kubectl get pods -l app=dbos-app
```

You should see the number of pods increase as KEDA detects the queue backlog. With `workerConcurrency: 1` and 10 enqueued workflows, you should see up to 10 pods.