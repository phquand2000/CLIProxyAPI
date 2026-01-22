# Letta Memory Injection Server

Custom CLIProxyAPI server với Letta memory injection middleware.

## Features

- **Pre-request:** Query Letta agent memory → Inject vào system prompt
- **Post-response:** Capture assistant response → Update Letta memory (async)
- **Fail-open:** Nếu Letta không available, request vẫn tiếp tục bình thường

## Quick Start (sử dụng pre-built binary)

```bash
# Download từ GitHub Releases
cd /path/to/proxypal/src-tauri/scripts
./download-letta-binaries.sh

# Hoặc download một platform cụ thể
./download-letta-binaries.sh cliproxyapi-aarch64-apple-darwin
```

## Build từ source

```bash
cd /Volumes/Data\ 2/My\ Project/CLIProxyAPI

# Build cho platform hiện tại
go build -o letta-cliproxyapi ./cmd/letta-server/

# Copy vào ProxyPal (macOS ARM64)
cp letta-cliproxyapi "/path/to/proxypal/src-tauri/binaries/cliproxyapi-aarch64-apple-darwin"
```

## CI/CD

Workflow tự động build khi push:
- `.github/workflows/build-letta-server.yml`
- Tạo releases với tag `letta-v{version}`
- Build cho: macOS (ARM64, x64), Linux (x64), Windows (x64)

## Configuration

Set environment variables:

```bash
# Enable Letta integration
export LETTA_ENABLED=true

# Letta server URL (default: http://localhost:8283)
export LETTA_SERVER_URL=http://localhost:8283

# Agent ID to use for memory
export LETTA_AGENT_ID=agent-b66fc4bb-1c1c-4898-af4e-778b5e0136b7

# Optional: config path (default: config.yaml)
export CONFIG_PATH=/path/to/config.yaml
```

Hoặc configure trong ProxyPal UI: **Settings > Letta Memory**

## Run standalone (for testing)

```bash
export LETTA_ENABLED=true
export LETTA_SERVER_URL=http://localhost:8283
export LETTA_AGENT_ID=agent-b66fc4bb-1c1c-4898-af4e-778b5e0136b7

./letta-cliproxyapi --config /path/to/config.yaml
```

## How it works

```
CLI Tools → ProxyPal (letta-cliproxyapi) → LLM Providers
                      ↓↑
                Letta Server (:8283)
```

1. Request đến `/v1/chat/completions`
2. Middleware query Letta memory (timeout 300ms)
3. Inject memory context vào system prompt
4. Forward request to upstream
5. Capture response
6. Async update Letta memory với conversation summary

## Memory injection format

```
--- Agent Memory Context ---
The following is context from your persistent memory. Use it to maintain continuity:

[persona]
<persona memory content>

[human]
<human memory content>

[project]
<project memory content>

--- End Memory Context ---
```

## Sync với upstream

```bash
# Add upstream remote
git remote add upstream https://github.com/router-for-me/CLIProxyAPI.git

# Fetch và merge
git fetch upstream
git merge upstream/main

# Resolve conflicts nếu có, rồi push
git push origin main
```
