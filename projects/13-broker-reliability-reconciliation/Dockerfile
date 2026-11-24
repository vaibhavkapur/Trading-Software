FROM golang:1.27-alpine AS builder
WORKDIR /app
COPY go.mod ./
COPY src/ ./src/
RUN CGO_ENABLED=0 GOOS=linux go build -o /recon ./src/cmd/recon

FROM alpine:3.20
WORKDIR /app
COPY --from=builder /recon /usr/local/bin/recon
COPY examples/ ./examples/
EXPOSE 8080
ENTRYPOINT ["recon", "serve", "-addr", ":8080", "-fixture", "examples/fixtures/paper_day.json", "-policy", "examples/policies/default_policy.json", "-state-dir", "/data/recon-state"]
