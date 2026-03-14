# Implementation Plan: IgnoreRegistered Feature

## Overview
Add a new configuration option `IgnoreRegistered` (integer, number of days) to the IRC bridge that ignores messages from authenticated accounts that are newer than the specified threshold. This complements the existing `IgnoreUnregistered` feature.

## Feature Requirements
- Configuration value: `IgnoreRegistered` (int) - number of days an account must have existed before messages are not ignored
- Uses IRCv3 `account-tag` capability (same infrastructure as `IgnoreUnregistered`)
- Tracks first-seen time for each authenticated account
- Ignores messages from accounts younger than threshold
- Respects channel filtering (similar to `IgnoreUnregistered`)
- Respects `UnregisteredWhitelist`

## Technical Design

### 1. Config Changes (`bridge/config/config.go`)
Add to `Protocol` struct:
```go
IgnoreRegistered int // irc, days to ignore recently registered accounts (requires account-tag)
```

### 2. IRC Bridge Changes (`bridge/irc/irc.go`)

#### 2.1 Add State Tracking
Add to `Birc` struct:
```go
type Birc struct {
    ...
    accountFirstSeen map[string]time.Time  // account name -> first seen timestamp
    accountMutex     sync.RWMutex           // protects the map (if needed)
}
```

Initialize in `New()`:
```go
b.accountFirstSeen = make(map[string]time.Time)
```

#### 2.2 Enable account-tag Capability
Modify `getClient()` around line 340-343:
```go
supportedCaps := map[string][]string{"overdrivenetworks.com/relaymsg": nil, "draft/relaymsg": nil}
if len(b.GetStringSlice("IgnoreUnregistered")) > 0 || b.GetInt("IgnoreRegistered") > 0 {
    supportedCaps["account-tag"] = nil
}
```

#### 2.3 Account Age Tracking
Choose a method to populate `accountFirstSeen`:

**Option A: Track in `handleOther()` from ACCOUNT tag** (recommended)
- When an `account` tag is received on a message, record first-seen time if not already set
- Tag processing already happens in `cap_tags.go` which calls `user.Extras.Account`
- Could add handler in irc.go to capture when accounts are first discovered

**Option B: Lazy initialization in `skipPrivMsg()`**
- On first message from an account, set `firstSeen = now`
- Simpler but may allow one message through before ignoring

**Option C: Use extended-join if available**
- If the server supports `extended-join`, account name is in JOIN params
- Could record when users join channels

**Decision:** Use Option A for consistency. The `account` tag is received on every message from an authenticated user. We'll add logic to initialize `firstSeen` on first encounter.

**Implementation approach:**
- Add a method like `b.getOrSetAccountFirstSeen(account string) time.Time`
- In `skipPrivMsg()`, call this before age check
- This method uses mutex to ensure thread-safe lazy initialization

#### 2.4 Modify `skipPrivMsg()` (line 384+)

After existing `IgnoreUnregistered` logic (lines 428-447), add:

```go
// IgnoreRegistered: ignore messages from very new accounts
ignoreDays := b.GetInt("IgnoreRegistered")
if ignoreDays > 0 && b.i.HasCapability("account-tag") {
    account, ok := event.Tags.Get("account")
    if ok {
        firstSeen := b.getOrSetAccountFirstSeen(account)
        ageDays := time.Since(firstSeen).Hours() / 24

        // Apply the same channel/gateway logic as IgnoreUnregistered?
        // For now, ignore if account is too new, regardless of channel,
        // but respect whitelist.
        if ageDays < float64(ignoreDays) {
            // Optional: check whitelist
            whitelist := b.GetStringSlice("UnregisteredWhitelist")
            for _, nick := range whitelist {
                if strings.EqualFold(nick, event.Source.Name) {
                    b.Log.Debugf("Allowing whitelisted nick %s (account %s, age %.1f days)", 
                        event.Source.Name, account, ageDays)
                    return false
                }
            }

            b.Log.Debugf("Ignoring message from %s (account %s, age %.1f days < %d)", 
                event.Source.Name, account, ageDays, ignoreDays)
            return true
        }
    }
}
```

#### 2.5 Thread Safety
- The `skipPrivMsg()` function is called from event handlers (`handleOther`)
- girc may call handlers concurrently
- Add mutex to protect `accountFirstSeen` map
- Implement getter/setter with locking

Add to `Birc`:
```go
accountMutex sync.RWMutex
```

Add method:
```go
func (b *Birc) getOrSetAccountFirstSeen(account string) time.Time {
    b.accountMutex.Lock()
    defer b.accountMutex.Unlock()

    if ts, exists := b.accountFirstSeen[account]; exists {
        return ts
    }
    now := time.Now()
    b.accountFirstSeen[account] = now
    return now
}
```

### 3. Optional Enhancements
- Periodic cleanup of `accountFirstSeen` map to prevent unbounded growth (e.g., cron job to remove entries older than 365 days)
- Configuration option to persist account cache across restarts (usually not needed)
- Support separate channel list (e.g., `IgnoreRegisteredChannels`) - but for now use same as `IgnoreUnregistered`

## Files to Modify
1. `bridge/config/config.go` - Add `IgnoreRegistered` field
2. `bridge/irc/irc.go` - Add tracking, capability, filtering logic

## Testing Strategy

### Unit Tests
1. Test `IgnoreRegistered` capability is requested when set
2. Test `skipPrivMsg()` ignores messages from new accounts (< threshold)
3. Test `skipPrivMsg()` does NOT ignore messages from old accounts (>= threshold)
4. Test whitelist bypass for `IgnoreRegistered`
5. Test thread-safety of account tracking map
6. Test behavior when `account-tag` not available
7. Test that `IgnoreUnregistered` and `IgnoreRegistered` work independently

### Manual Testing
- Use a test IRC server with SASL authentication
- Configure two accounts: one registered today, one registered >N days ago
- Verify the new account's messages are dropped, old account's pass through

## Edge Cases & Considerations

| Case | Expected Behavior |
|------|-------------------|
| `IgnoreRegistered` = 0 | Disabled (no effect) |
| No `account-tag` capability | Feature gracefully disabled (only works if cap available) |
| Account seen for first time | Assume "now" as firstSeen → will be ignored until threshold passes |
| Account name collision (impossible) | Not a concern - account names are globally unique |
| Bridge restart | FirstSeen resets → may temporarily allow new accounts until age threshold re-accumulates |
| Time zone / clock changes | Uses monotonic time; no issues with wall clock changes |
| Multiple gateways/channels | Same account state applies globally to bridge instance |

## Tradeoffs Made
- Shared channel list with `IgnoreUnregistered` for simplicity
- Whitelist applies to both unregistered and registered filters for consistency
- Lazy initialization allows first message through (rare edge case, acceptable)
- No persistence - bridge restart resets counters (could be added later if needed)

## Estimated Effort
- **Complexity:** Medium
- **Files changed:** ~2-3 (config + irc bridge)
- **Lines added:** ~50-80
