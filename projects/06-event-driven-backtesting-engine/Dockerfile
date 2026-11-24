FROM python:3.12-slim

WORKDIR /app
COPY pyproject.toml README.md ./
COPY src ./src
COPY examples ./examples
RUN pip install --no-cache-dir . \
    && useradd --create-home backtester \
    && mkdir -p /app/data /app/runs \
    && chown backtester /app/data /app/runs
USER backtester

# Offline by design: the engine reads local files and writes local reports.
CMD ["python", "-m", "backtester.demo"]
