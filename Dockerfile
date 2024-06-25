FROM golang:1.22.4-alpine3.19

RUN go install github.com/air-verse/air@latest && \
    go install github.com/go-delve/delve/cmd/dlv@latest

RUN apk add --no-cache git
