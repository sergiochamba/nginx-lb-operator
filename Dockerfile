FROM golang:1.17 as builder
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download

COPY main.go ./
COPY controllers/ controllers/
COPY pkg/ pkg/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o manager main.go

FROM alpine:latest
WORKDIR /
COPY --from=builder /workspace/manager .
ENTRYPOINT ["/manager"]