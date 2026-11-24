FROM python:3.12-slim

WORKDIR /app
COPY pyproject.toml ./
COPY src ./src
COPY examples/data ./examples/data
RUN pip install --no-cache-dir . && useradd --create-home tca && mkdir /data && chown tca /data
USER tca

ENV TCA_HOST=0.0.0.0 TCA_PORT=8009 TCA_DB_PATH=/data/tca.sqlite3
EXPOSE 8009
CMD ["python", "-m", "tca", "serve", "--load", "examples/data"]
