FROM golang:1.23.2 AS builder
WORKDIR /workspace

# Copy and download dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY main.go ./
COPY controllers/ controllers/
COPY utils/ utils/

# Build the Go binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o manager main.go

# Final stage
FROM gcr.io/distroless/static:nonroot

COPY --from=builder /workspace/manager /

USER nonroot:nonroot

ENTRYPOINT ["/manager"]
