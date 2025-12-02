# Autoscale a DBOS application on Kubernetes with KEDA

Queues are the prime mechanism to control load in a DBOS application. For example you can set a per-worker concurrency cap on a DBOS queue, controlling how many tasks a single worker can dequeue. You can estimate how many workers are required at any given time to handle a queue's tasks with:

**number of items in the queue / per worker concurrency**

In this tutorial, we walk you through configuring KEDA on Kubernetes to scale pods based on DBOS queue utilization, using the metric API.

## The application

For this tutorial we'll use an application with a single queues with a worker concurrency set. The application will expose an endpoint which will enqueue a single workflow, set to sleep for a configurable duration.

The code snippets will be in Golang but the concept works across all DBOS SDKs.

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

### Deploy a DBOS application

Update this manifest with your specific values. Note that your load balancer configuration may vary based on your Kubernetes setup.

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
          image: YOUR_IMAGE_NAME
          env:
            - name: DBOS_SYSTEM_DATABASE_URL
              value: postgres://postgres:dbos@postgres:5432/kube
          ports:
            - containerPort: 8000
---
apiVersion: v1
kind: Service
metadata:
  name: dbos-app
spec:
  type: LoadBalancer
  selector:
    app: dbos-app
  ports:
    - port: 8000
      targetPort: 8000
```

### Configure a KEDA scaled object

Now we will instruct KEDA to scale our application's pods based on a queue utilization metric exposed by the application itself. Update this manifest with your service URL.

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: dbos-app-scaledobject
spec:
  scaleTargetRef:
    name: dbos-app
  minReplicaCount: 1
  maxReplicaCount: 100
  triggers:
  - type: metrics-api
    metadata:
      url: http://dbos-app.default.svc.cluster.local:8000/metrics
      valueLocation: expected_pods
      targetValue: "1"
```

The `valueLocation` field represents a JSON field in the /metrics endpoint response, where we expect the metric's value to reside. `targetValue: "1"` means we map 1:1 the metric's value to the desired pods number. 

## The metrics endpoint

We will now program an endpoint in the application that returns the number of workers needed to handle the busiest queue's load. You can of course change this logic for any metric of your choice (e.g., target a specific queue, or sum across all queues.)

The endpoint:
1. Access the DBOS Context queue registry to list all the registered queues
2. Retain queues that have an explicit "worker concurrency" property
3. List all queue workflows
4. Compute the load ratio for each queue
5. Return the maximum load ratio

[ Show code here ]


## Try it

Using the endpoint in the application, enqueue a number of workflows that exceeds the concurrency limit, for example:

```bash
# Enqueue 10 workflows that sleep for 30 seconds each
for i in {1..10}; do curl -s http://YOUR_LOAD_BALANCER:8000/enqueue/30 & done;
```

Watch the pods scale up:

```bash
# Watch pods in real-time
watch -n 1 kubectl get pods -l app=dbos-app
```

You should see the number of pods increase as KEDA detects the queue backlog. With `worker_concurrency=1` and 10 enqueued workflows, you should see up to 10 pods.

Pods will eventually scale down (per KEDA configuration)