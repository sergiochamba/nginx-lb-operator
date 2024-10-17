FROM golang:1.17 as builder
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download

COPY main.go ./
COPY controllers/ controllers/
COPY pkg/ pkg/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o manager main.go

FROM alpine:latest
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /workspace/manager .
COPY pkg/nginx/templates/ templates/
ENTRYPOINT ["/app/manager"]