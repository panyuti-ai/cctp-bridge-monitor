# CCTP Bridge Monitor (V1)

A real-time cross-chain USDC transfer monitoring dashboard built on [Circle's Cross-Chain Transfer Protocol (CCTP) V1](https://www.circle.com/cross-chain-transfer-protocol).

Monitors all `MessageSent` and `MessageReceived` events across 7 chains simultaneously, pairs them together, and exposes a live dashboard with transfer status and statistics.

## Demo

> Dashboard running live at: `http://YOUR_SERVER:8080`

![dashboard preview](https://i.imgur.com/placeholder.png)

---

## Supported Chains

| Chain | CCTP Domain | Transport |
|-------|-------------|-----------|
| Ethereum | 0 | WebSocket (go-ethereum) |
| Avalanche | 1 | WebSocket (go-ethereum) |
| Optimism | 2 | WebSocket (go-ethereum) |
| Arbitrum | 3 | WebSocket (go-ethereum) |
| Solana | 5 | HTTP polling (5s interval) |
| Base | 6 | WebSocket (go-ethereum) |
| Polygon | 7 | WebSocket (go-ethereum) |

Noble (domain 4) is intentionally excluded — CCTP messages destined for Noble are filtered out as Noble is a Cosmos chain without public EVM-compatible RPC support.

---

## Architecture

```
┌─────────────────────────────────────────────────────────┐
│                      cmd/monitor                         │
│  Loads config, initialises all indexers, starts server  │
└────────────┬────────────────────────────────────────────┘
             │  chan indexer.Event (buffered, shared)
     ┌───────▼────────┐        ┌──────────────────────┐
     │  EVM Indexers  │        │   Solana Indexer     │
     │  (×6 chains)   │        │  HTTP polling, Borsh │
     │  WebSocket sub │        │  decode Anchor events│
     └───────┬────────┘        └──────────┬───────────┘
             └──────────┬─────────────────┘
                  ┌─────▼──────┐
                  │   Matcher  │
                  │ MessageSent↔MessageReceived pairing │
                  │ 30s retry for out-of-order events  │
                  └─────┬──────┘
                  ┌─────▼──────┐
                  │  SQLite DB │
                  │ transfers  │
                  │ checkpoints│
                  └─────┬──────┘
                  ┌─────▼──────┐
                  │ Gin HTTP   │
                  │ /api/transfers │
                  │ /api/stats     │
                  │ /  (dashboard) │
                  └────────────┘
```

### Key Design Decisions

**Single shared event channel** — all 7 indexers push into one `chan indexer.Event`. The matcher is the sole consumer, eliminating any need for locking.

**Out-of-order event handling** — `MessageReceived` can arrive before `MessageSent` is persisted (especially when indexers start at different block heights). The matcher buffers unmatched `MessageReceived` events and retries every 30 seconds, dropping them after 2 hours.

**Checkpoint-based resume** — each chain saves the latest processed block to SQLite. On restart, the indexer scans forward from the checkpoint so no events are missed.

**Reconnect gap fill** — if a WebSocket subscription drops, the indexer tracks the last seen block and back-fills the gap before re-subscribing.

**Rate-limit aware scanning** — historical block scan uses exponential backoff retry and a 150ms inter-batch delay to stay within free-tier RPC limits (Alchemy free: max 10 blocks per `eth_getLogs`).

---

## Getting Started

### Prerequisites

- Go 1.21+
- An [Alchemy](https://www.alchemy.com/) account (free tier works)

### Setup

```bash
git clone https://github.com/YOUR_USERNAME/cctp-bridge-monitor
cd cctp-bridge-monitor

cp .env.example .env
# Fill in your RPC URLs in .env
```

`.env` format:

```env
ETH_RPC_URL=wss://eth-mainnet.g.alchemy.com/v2/YOUR_KEY
BASE_RPC_URL=wss://base-mainnet.g.alchemy.com/v2/YOUR_KEY
AVAX_RPC_URL=wss://avax-mainnet.g.alchemy.com/v2/YOUR_KEY
OP_RPC_URL=wss://opt-mainnet.g.alchemy.com/v2/YOUR_KEY
ARB_RPC_URL=wss://arb-mainnet.g.alchemy.com/v2/YOUR_KEY
POLYGON_RPC_URL=wss://polygon-mainnet.g.alchemy.com/v2/YOUR_KEY
SOL_RPC_URL=https://solana-mainnet.g.alchemy.com/v2/YOUR_KEY
```

Chains with no RPC URL set are silently skipped — you can start with just one chain.

### Run

```bash
go run ./cmd/monitor/
```

Dashboard available at `http://localhost:8080`.

---

## API

| Endpoint | Description |
|----------|-------------|
| `GET /api/transfers` | All transfers, newest first |
| `GET /api/transfers/:nonce` | Single transfer by nonce |
| `GET /api/stats` | Total / completed / pending counts + avg duration |
| `GET /` | Live dashboard (HTML) |

---

## Project Structure

```
cmd/monitor/main.go          — entry point, chain config, startup
internal/
  indexer/
    indexer.go               — EVM indexer (WebSocket, go-ethereum)
    solana_indexer.go        — Solana indexer (HTTP polling, Borsh decode)
  matcher/matcher.go         — pairs MessageSent ↔ MessageReceived
  db/
    db.go                    — SQLite operations
    models.go                — Transfer struct, TransferStatus
  api/api.go                 — Gin HTTP server
web/index.html               — single-page dashboard (vanilla JS)
```

---

## What's Next — V2

This repository tracks CCTP V1. A separate [cctp-v2-monitor](#) will extend this to support **CCTP V2 Fast Transfers**, including:

- Fast Transfer hook events
- `finalityThreshold` parameter tracking
- Sub-minute settlement detection

---

## License

MIT
