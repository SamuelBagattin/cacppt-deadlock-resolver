FROM golang:1.23-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-w -s -X main.version=${VERSION}" -o /cacppt-deadlock-resolver .

FROM alpine:3.21

ARG TALOSCTL_VERSION=v1.12.2

RUN apk add --no-cache ca-certificates curl \
    && curl -fsSL "https://github.com/siderolabs/talos/releases/download/${TALOSCTL_VERSION}/talosctl-linux-amd64" \
       -o /usr/local/bin/talosctl && chmod +x /usr/local/bin/talosctl \
    && apk del curl

COPY --from=builder /cacppt-deadlock-resolver /usr/local/bin/cacppt-deadlock-resolver

USER 65534:65534
ENTRYPOINT ["/usr/local/bin/cacppt-deadlock-resolver"]
