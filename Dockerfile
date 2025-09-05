# Multi-stage build for optimized image size
FROM python:3.11-slim as builder

# Set build arguments
ARG DEBIAN_FRONTEND=noninteractive

# Install build dependencies
RUN apt-get update && apt-get install -y \
    build-essential \
    git \
    curl \
    && rm -rf /var/lib/apt/lists/*

# Create virtual environment
RUN python -m venv /opt/venv
ENV PATH="/opt/venv/bin:$PATH"

# Copy requirements first for better caching
COPY requirements.txt requirements-dev.txt ./
RUN pip install --no-cache-dir --upgrade pip \
    && pip install --no-cache-dir -r requirements.txt

# Production stage
FROM python:3.11-slim

# Set runtime arguments
ARG DEBIAN_FRONTEND=noninteractive

# Install runtime dependencies
RUN apt-get update && apt-get install -y \
    # Audio system dependencies
    alsa-utils \
    pulseaudio-utils \
    sox \
    # Git for potential Claude Code requirements
    git \
    # Process utilities
    procps \
    # Cleanup
    && rm -rf /var/lib/apt/lists/* \
    && apt-get clean

# Copy virtual environment from builder
COPY --from=builder /opt/venv /opt/venv
ENV PATH="/opt/venv/bin:$PATH"

# Create application user for security
RUN groupadd -r appuser && useradd -r -g appuser -s /bin/false appuser

# Set working directory
WORKDIR /app

# Copy application code
COPY src/ src/
COPY pyproject.toml README.md ./

# Install the application in development mode
RUN pip install -e .

# Create directories with proper permissions
RUN mkdir -p data prompts config logs \
    && chown -R appuser:appuser /app

# Copy example configurations
COPY config/config.yaml.example config/
COPY .env.example ./

# Create a default prompt file if none exists
RUN mkdir -p prompts && echo "# Default prompt file" > prompts/review_prompt.txt \
    && chown -R appuser:appuser prompts

# Switch to non-root user
USER appuser

# Health check
HEALTHCHECK --interval=60s --timeout=10s --start-period=30s --retries=3 \
    CMD python -c "import sqlite3; sqlite3.connect('data/reviews.db').execute('SELECT 1').fetchone()" || exit 1

# Set default environment variables
ENV PYTHONPATH=/app/src
ENV LOG_LEVEL=INFO
ENV DATABASE_PATH=data/reviews.db
ENV SOUND_ENABLED=false

# Expose volume for persistent data
VOLUME ["/app/data", "/app/prompts", "/app/config"]

# Default command
CMD ["code-reviewer"]