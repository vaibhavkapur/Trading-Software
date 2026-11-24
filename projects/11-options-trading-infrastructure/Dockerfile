FROM python:3.12-slim

WORKDIR /app

COPY pyproject.toml README.md LICENSE ./
COPY src ./src
COPY examples ./examples

RUN pip install --no-cache-dir .

EXPOSE 8011

ENV OPTIONS_INFRA_DB_PATH=/app/data/paper_orders.sqlite3
ENV OPTIONS_INFRA_FIXTURE_PATH=/app/examples/fixtures/xyz_chain_2027_01_15.json

CMD ["uvicorn", "options_infra.api.app:app", "--host", "0.0.0.0", "--port", "8011"]
