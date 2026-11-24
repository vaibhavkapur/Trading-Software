FROM golang:1.27.1 AS build
WORKDIR /src
COPY go.mod ./
COPY src ./src
RUN CGO_ENABLED=0 go build -o /surveild ./src/cmd/surveild && mkdir /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /surveild /surveild
COPY examples/rules.json /etc/surveillance/rules.json
COPY examples/refdata.json /etc/surveillance/refdata.json
# Pre-create the data directory owned by the unprivileged user so a fresh
# named volume is writable.
COPY --from=build --chown=nonroot:nonroot /data /data
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/surveild", "-addr", "0.0.0.0:8080", "-rules", "/etc/surveillance/rules.json", "-refdata", "/etc/surveillance/refdata.json", "-data", "/data"]
