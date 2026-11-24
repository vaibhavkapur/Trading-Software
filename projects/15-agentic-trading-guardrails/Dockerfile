FROM golang:1.24-alpine AS builder
WORKDIR /app
COPY go.mod ./
COPY src ./src
COPY examples ./examples
RUN CGO_ENABLED=0 go build -o /out/gateway ./src/cmd/gateway && \
    CGO_ENABLED=0 go build -o /out/demo ./src/cmd/demo

FROM alpine:3.21
RUN adduser -D -H guardrails
WORKDIR /app
COPY --from=builder /out/gateway /usr/local/bin/gateway
COPY --from=builder /out/demo /usr/local/bin/demo
COPY examples ./examples
RUN mkdir -p /data && chown guardrails:guardrails /data
USER guardrails
EXPOSE 8080
ENV GATEWAY_ADDR=":8080" \
    GUARDRAILS_DATA_DIR="/data" \
    GUARDRAILS_POLICY_FILE="/app/examples/policies/default-policy.json" \
    BROKER_MODE="PAPER"
ENTRYPOINT ["/usr/local/bin/gateway"]
