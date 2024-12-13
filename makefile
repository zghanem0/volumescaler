# Variables
REGION = us-east-1
ACCOUNT_ID := $(shell aws sts get-caller-identity --query Account --output text)

# Construct the ECR Repository URI using the fetched Account ID
REPO_URI = $(ACCOUNT_ID).dkr.ecr.$(REGION).amazonaws.com
IMAGE_NAME = volumescaler
TAG = latest

.PHONY: deploy

deploy:
	@echo "Fetching AWS Account ID..."
	@echo "Using Account ID: $(ACCOUNT_ID)"

	# Login to AWS ECR using Docker with sudo to avoid permission issues
	@echo "Logging into AWS ECR..."
	aws ecr get-login-password --region $(REGION) | sudo docker login --username AWS --password-stdin $(REPO_URI)

	# Build the Docker image with sudo
	@echo "Building Docker image: $(REPO_URI)/$(IMAGE_NAME):$(TAG)"
	sudo docker build -t $(REPO_URI)/$(IMAGE_NAME):$(TAG) .

	# Push the Docker image to ECR with sudo
	@echo "Pushing Docker image to ECR..."
	sudo docker push $(REPO_URI)/$(IMAGE_NAME):$(TAG)

	@echo "Deployment completed successfully!"