FROM golang:1.25-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-w -s -X main.version=${VERSION}" -o /cacppt-deadlock-resolver .

FROM alpine:3.21

RUN apk add --no-cache ca-certificates

COPY --from=builder /cacppt-deadlock-resolver /usr/local/bin/cacppt-deadlock-resolver

USER 65534:65534
ENTRYPOINT ["/usr/local/bin/cacppt-deadlock-resolver"]
