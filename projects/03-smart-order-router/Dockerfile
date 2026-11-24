FROM golang:1.27.1-alpine AS build
WORKDIR /app
COPY go.mod ./
COPY src ./src
RUN CGO_ENABLED=0 go build -trimpath -o /sor ./src/cmd/sor

FROM scratch
COPY --from=build /sor /sor
COPY examples /examples
USER 65534
EXPOSE 8080
ENTRYPOINT ["/sor"]
CMD ["serve", "-addr", "0.0.0.0:8080", "-speed", "20"]
