# Builds the order management service and the simulated broker.
# SIMULATED / PAPER TRADING ONLY.
FROM golang:1.27.1-alpine AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY src ./src
RUN CGO_ENABLED=0 go build -trimpath -o /out/omsd ./src/cmd/omsd \
 && CGO_ENABLED=0 go build -trimpath -o /out/brokersim ./src/cmd/brokersim

FROM scratch
COPY --from=build /out/omsd /omsd
COPY --from=build /out/brokersim /brokersim
COPY examples/risk-limits.json /etc/oms/risk-limits.json
USER 65534:65534
EXPOSE 8080
ENTRYPOINT ["/omsd"]
CMD ["serve"]
