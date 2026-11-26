FROM golang:1.25.1-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

ARG GO_BUILD_TAGS=""
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags "${GO_BUILD_TAGS}" -o /out/billing-service ./cmd/billing-service

FROM alpine:3.22

RUN addgroup -S app && adduser -S app -G app

WORKDIR /app

COPY --from=builder /out/billing-service /app/billing-service

USER app

EXPOSE 8081

ENTRYPOINT ["/app/billing-service"]
