FROM python:3.11-slim

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PIP_NO_CACHE_DIR=1

WORKDIR /srv

# Install runtime deps first for better layer caching.
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY app ./app

EXPOSE 8200

# Healthcheck hits the lightweight /healthz endpoint.
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD python -c "import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('http://localhost:8200/healthz').status==200 else 1)"

# Trivy DS-0002 requires a non-root USER directive. The eval-service is a
# test-only fake LLM evaluator; running as nobody inside a CI-only network
# removes the trivy scanner warning without changing its behaviour.
RUN useradd -r -u 65532 -m -d /srv eval || true
USER 65532

CMD ["uvicorn", "app.main:app", "--host", "0.0.0.0", "--port", "8200", "--workers", "2"] 
