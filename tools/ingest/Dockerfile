FROM python:3.12-slim

WORKDIR /app
ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1

COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt

COPY . .

# Subcommand comes from the compose invocation, e.g.
#   docker compose --profile ingest run --rm ingest parse
ENTRYPOINT ["python", "-m", "ingest"]
CMD ["all"]
