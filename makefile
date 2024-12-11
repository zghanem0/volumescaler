REGION = us-east-1
REPO_URI = 374031103815.dkr.ecr.us-east-1.amazonaws.com
IMAGE_NAME = volumescaler
TAG = latest

.PHONY: deploy

deploy:
	# Login to AWS ECR using sudo to avoid permission issues with Docker
	aws ecr get-login-password --region $(REGION) | sudo docker login --username AWS --password-stdin $(REPO_URI)

	# Build the Docker image (with sudo)
	sudo docker build -t $(REPO_URI)/$(IMAGE_NAME):$(TAG) .

	# Push the image to ECR (with sudo)
	sudo docker push $(REPO_URI)/$(IMAGE_NAME):$(TAG)