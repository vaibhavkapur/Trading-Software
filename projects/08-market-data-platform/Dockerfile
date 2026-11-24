FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY src ./src
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mdp ./src/cmd/mdp

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/mdp /usr/local/bin/mdp
# Reference data and the simulated capture; no credentials are baked in.
COPY examples ./examples
USER nonroot
ENTRYPOINT ["/usr/local/bin/mdp"]
