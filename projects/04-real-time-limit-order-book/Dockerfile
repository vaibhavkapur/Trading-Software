# syntax=docker/dockerfile:1
FROM node:20-bookworm-slim AS ui
WORKDIR /ui
COPY src/ui/package.json src/ui/package-lock.json ./
RUN npm ci
COPY src/ui ./
RUN npm run build

FROM rust:1.99-bookworm AS build
WORKDIR /app
COPY Cargo.toml Cargo.lock rust-toolchain.toml ./
COPY src ./src
COPY tests ./tests
RUN cargo build --release --locked -p stream-api --bin lob

FROM debian:bookworm-slim
RUN useradd --system --uid 10001 lob
WORKDIR /app
COPY --from=build /app/target/release/lob /usr/local/bin/lob
COPY --from=ui /ui/dist ./ui
COPY examples ./examples
USER lob
EXPOSE 8080
ENTRYPOINT ["lob"]
CMD ["serve", "--addr", "0.0.0.0:8080", "--ui-dir", "/app/ui"]
