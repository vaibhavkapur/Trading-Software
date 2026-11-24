# Simulated matching engine gateway. Not for real-money use.
FROM rust:1.99-slim AS build
WORKDIR /src
COPY Cargo.toml Cargo.lock build.rs ./
COPY src ./src
COPY benches ./benches
COPY tests ./tests
RUN cargo build --locked --release --bin me

FROM debian:bookworm-slim
RUN useradd --system --create-home --uid 10001 engine \
    && mkdir /data && chown engine /data
COPY --from=build /src/target/release/me /usr/local/bin/me
USER engine
VOLUME /data
EXPOSE 9001
ENTRYPOINT ["me"]
CMD ["serve", "--addr", "0.0.0.0:9001", "--data-dir", "/data"]
