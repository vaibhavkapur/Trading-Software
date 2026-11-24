FROM python:3.12-slim

WORKDIR /app
COPY pyproject.toml README.md ./
COPY src ./src
RUN pip install --no-cache-dir ".[llm]" && useradd --create-home atp && mkdir /data && chown atp /data
USER atp

ENV ATP_HOST=0.0.0.0 ATP_PORT=8000 ATP_DB_PATH=/data/atp.sqlite3
EXPOSE 8000
CMD ["python", "-m", "atp"]
