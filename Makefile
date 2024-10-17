# Variables
IMAGE_NAME ?= sergiochamba/nginx-lb-operator:latest

# Build the operator binary
build:
	go build -o bin/manager main.go

# Run the operator locally
run: generate fmt vet
	go run ./main.go

# Generate code
generate:
	operator-sdk generate k8s

# Format code
fmt:
	go fmt ./...

# Vet code
vet:
	go vet ./...

# Build Docker image
docker-build:
#    docker build -t ${IMAGE_NAME} .
# Build Docker image with buildx (when using Apple Silicon)
	DOCKER_CLI_EXPERIMENTAL=enabled docker buildx build --platform linux/amd64 -t ${IMAGE_NAME} .

# Push Docker image
docker-push:
	docker push ${IMAGE_NAME}

# Deploy operator to cluster
deploy:
	kubectl apply -f config/

# Undeploy operator from cluster
undeploy:
	kubectl delete -f config/