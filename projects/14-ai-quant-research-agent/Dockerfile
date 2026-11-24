FROM python:3.12-slim

WORKDIR /app

COPY pyproject.toml README.md LICENSE ./
COPY src ./src
COPY examples ./examples
COPY scripts ./scripts

RUN pip install --no-cache-dir .

ENV QUANT_AGENT_DATA_DIR=/app/data
ENV QUANT_AGENT_MODE=PAPER_SIMULATED

EXPOSE 8014

CMD ["uvicorn", "quant_agent.api.app:app", "--host", "0.0.0.0", "--port", "8014"]
