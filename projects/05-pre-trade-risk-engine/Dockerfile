FROM golang:1.27.1 AS build
WORKDIR /src
COPY go.mod ./
COPY src ./src
RUN CGO_ENABLED=0 go build -o /riskd ./src/cmd/riskd && mkdir /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /riskd /riskd
COPY examples/policy.json /etc/risk/policy.json
# Pre-create the data directory owned by the unprivileged user so a fresh
# named volume is writable.
COPY --from=build --chown=nonroot:nonroot /data /data
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/riskd", "-addr", "0.0.0.0:8080", "-policy", "/etc/risk/policy.json", "-data", "/data"]
