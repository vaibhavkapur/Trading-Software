FROM python:3.12-slim

WORKDIR /app

COPY pyproject.toml README.md ./
COPY src ./src
COPY config ./config
COPY data ./data
COPY scripts ./scripts

RUN pip install --no-cache-dir .

ENV PATP_MODE=PAPER
ENV PATP_DATA_DIR=/app/data/runtime
EXPOSE 8080

CMD ["patp", "serve", "--host", "0.0.0.0", "--port", "8080"]
