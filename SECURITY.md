# API Key Security Guide

This guide explains how to set up and manage API key authentication for the IEEE Office Backend System.

## Overview

The system supports API key authentication to secure the backend API endpoints. This prevents unauthorized access from unknown clients and protects against abuse.

## Architecture

- **Backend (Go)**: Validates API keys via `X-API-Key` HTTP header
- **Discord Bot (Python)**: Sends API key with all requests
- **ESP32 Scanner (C++)**: Sends API key with scan requests

## Generating API Keys

### Using OpenSSL (Recommended)

Generate a secure random API key:

```bash
# Generate a 32-byte (256-bit) API key
openssl rand -base64 32
```

### Using Python

```python
import secrets
import base64

# Generate a secure API key
api_key = base64.b64encode(secrets.token_bytes(32)).decode('utf-8')
print(api_key)
```

### Using a Password Manager

Most password managers (1Password, LastPass, Bitwarden) can generate secure random strings. Use a length of 32+ characters.

## Configuration

### Backend Configuration

Set environmental variables using a `.env` file and a tool like `direnv` or `dotenv`, or export them in your shell before running the server (e.g., `export SCANNER_API_KEY=yourkey`). The Docker Compose setup automatically loads from `.env`.

```bash
export SCANNER_API_KEY=your_scanner_api_key_here
export DISCORD_BOT_API_KEY=your_discord_bot_api_key_here
export API_KEYS=key1,key2,key3
```

**Important**:

- If NO API keys are configured, the backend operates in **open mode** (all requests allowed)
- If ANY API key is configured, authentication is **required** for all endpoints (except `/health`)

### Discord Bot Configuration

Edit `office-tracker/.env`:

```bash
DISCORD_BOT_API_KEY=your_discord_bot_api_key_here
```

The key must match one of the keys configured in the backend.

### ESP32 Scanner Configuration

Edit `office-tracker/lib/secrets/secrets.h`:

```cpp
#define API_KEY "your_scanner_api_key_here"
```

Leave empty (`""`) if not using API key authentication.
