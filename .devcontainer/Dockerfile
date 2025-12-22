FROM golang:1.25.3-bookworm

# Install basic development tools and sqlite3 for DB inspection
RUN apt-get update && apt-get install -y \
    sqlite3 \
    git \
    curl \
    && rm -rf /var/lib/apt/lists/*
