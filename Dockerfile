FROM golang:1.23.2 AS builder
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download

COPY main.go ./
COPY controllers/ controllers/
COPY pkg/ pkg/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o manager main.go

FROM gcr.io/distroless/static:nonroot
WORKDIR /app
COPY --from=builder /workspace/manager .
COPY pkg/nginx/templates/ templates/
USER nonroot:nonroot
ENTRYPOINT ["/app/manager"]
