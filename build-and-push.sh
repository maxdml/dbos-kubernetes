#!/bin/bash

# Configuration
# Update these values with your ECR repository details
ECR_REPO="YOUR_ACCOUNT.dkr.ecr.YOUR_REGION.amazonaws.com/YOUR_REPO"
IMAGE_NAME="kubernetes-integration"
AWS_REGION="YOUR_REGION"

# Get the image tag (default to latest if not provided)
TAG=${1:-latest}
FULL_IMAGE_NAME="${ECR_REPO}:${IMAGE_NAME}-${TAG}"

echo "Building Docker image..."
docker build -t ${IMAGE_NAME}:${TAG} .

if [ $? -ne 0 ]; then
    echo "Docker build failed!"
    exit 1
fi

echo "Tagging image for ECR..."
docker tag ${IMAGE_NAME}:${TAG} ${FULL_IMAGE_NAME}

echo "Logging in to ECR..."
aws ecr get-login-password --region ${AWS_REGION} | docker login --username AWS --password-stdin ${ECR_REPO}

if [ $? -ne 0 ]; then
    echo "ECR login failed! Make sure you have AWS credentials configured."
    exit 1
fi

echo "Pushing image to ECR..."
docker push ${FULL_IMAGE_NAME}

if [ $? -ne 0 ]; then
    echo "Docker push failed!"
    exit 1
fi

echo "Successfully pushed ${FULL_IMAGE_NAME}"

