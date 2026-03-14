# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Matterbridge is a Go chat bridge that relays messages between 20+ communication platforms (Discord, IRC, Slack, Telegram, Matrix, Mattermost, WhatsApp, XMPP, Zulip, etc.) in real time. It runs as a single binary configured via TOML/YAML/JSON.

## Build & Development Commands

```bash
# Build (Linux amd64)
./make.sh

# Test
go test ./... -mod=vendor

# Test a single package
go test ./gateway/ -mod=vendor
go test ./bridge/discord/ -mod=vendor

# Lint (CI uses --new-from-rev HEAD~5)
golangci-lint run -v

# Run
go run . -conf matterbridge.toml
go run . -conf matterbridge.toml -debug  # with debug logging
```

All dependencies are vendored (`-mod=vendor` is required). Build tags: `whatsappmulti` (WhatsApp multidevice), `nomsteams`/`nozulip` (exclude Teams/Zulip to lower RAM).

## Architecture

### Message Flow

```
Protocol (e.g. Discord) → Bridge → Router → Gateway → destination Bridge(es) → Protocol(s)
```

### Core Components

- **`matterbridge.go`** — Entry point. Parses flags (`-conf`, `-debug`, `-version`, `-gops`), sets up logging, creates the Router.
- **`gateway/router.go`** — Orchestrates all Gateways. Receives messages on a central channel, routes them to the correct Gateway(s). Maintains an LRU message ID cache for edit/delete tracking.
- **`gateway/gateway.go`** — Manages channel mappings between bridges. Handles message transformation (RemoteNickFormat, ReplaceMessages, etc.), file uploads/downloads, and routing to destination bridges.
- **`gateway/dynamic.go`** — Wildcard channel support (`channel = "*"`). Handles EventChannelCreate/Delete for dynamic bridging. Maps channel names across protocols (e.g. Discord `_channel` ↔ IRC `#channel`).
- **`bridge/bridge.go`** — Base struct for all protocol bridges. Wraps config access with helper methods.
- **`bridge/config/config.go`** — Configuration loading (Viper-based), all type definitions: `Message`, `Protocol`, `Gateway`, `ChannelInfo`, etc.

### Protocol Bridges

Each protocol lives in `bridge/<protocol>/` and implements the `Bridger` interface:

```go
type Bridger interface {
    Send(msg config.Message) (string, error)
    Connect() error
    JoinChannel(channel config.ChannelInfo) error
    PartChannel(channel config.ChannelInfo) error
    Disconnect() error
}
```

Optional interface for dynamic bridging:
```go
type ExistingChannelLister interface {
    GetExistingChannels() []string
}
```

### Bridge Registration

`gateway/bridgemap/` contains one file per protocol (e.g. `bdiscord.go`, `birc.go`) that registers a factory function. The central `bridgemap.go` aggregates them all into a map used by the Router.

### Configuration Structure

```toml
[irc.libera]
  Server = "irc.libera.chat:6697"
  Nick = "mybot"

[discord.myserver]
  Token = "..."

[[gateway]]
name = "mygateway"
enable = true
  [[gateway.inout]]
  account = "irc.libera"
  channel = "#channel"

  [[gateway.inout]]
  account = "discord.myserver"
  channel = "ID:123456789"
```

Accounts are referenced as `protocol.instance` (e.g. `irc.libera`, `discord.myserver`). Channel directions: `in` (receive only), `out` (send only), `inout` (both).

## Linting

Uses golangci-lint with `enable-all: true` and a long disable list. Config in `.golangci.yaml`. The `gateway/bridgemap/` directory is excluded from linting. CI runs lint only on recent changes (`--new-from-rev HEAD~5`).

## Current Branch Work (`mine`)

Dynamic channel bridging: wildcard channels, auto-join/part, `IgnoreUnregistered` with wildcard support, and `UnregisteredWhitelist`. Key files: `gateway/dynamic.go`, `gateway/dynamic_test.go`, and protocol-level changes in `bridge/discord/` and `bridge/irc/`.
