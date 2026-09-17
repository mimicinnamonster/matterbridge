package birc

import (
	"testing"
	"time"

	"github.com/42wim/matterbridge/bridge"
	"github.com/42wim/matterbridge/bridge/config"
	"github.com/lrstanley/girc"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// test helpers
// ---------------------------------------------------------------------------

// fakeConfig is a minimal in-memory implementation of the config.Config
// interface used to drive Birc in tests without loading a TOML file.
type fakeConfig struct {
	overrides map[string]interface{}
}

func (f *fakeConfig) Viper() *viper.Viper { return nil }
func (f *fakeConfig) BridgeValues() *config.BridgeValues {
	return &config.BridgeValues{}
}
func (f *fakeConfig) IsKeySet(key string) bool {
	_, ok := f.overrides[key]
	return ok
}
func (f *fakeConfig) GetBool(key string) (bool, bool) {
	if v, ok := f.overrides[key].(bool); ok {
		return v, true
	}
	return false, false
}
func (f *fakeConfig) GetInt(key string) (int, bool) {
	if v, ok := f.overrides[key].(int); ok {
		return v, true
	}
	return 0, false
}
func (f *fakeConfig) GetString(key string) (string, bool) {
	if v, ok := f.overrides[key].(string); ok {
		return v, true
	}
	return "", false
}
func (f *fakeConfig) GetStringSlice(key string) ([]string, bool) {
	if v, ok := f.overrides[key].([]string); ok {
		return v, true
	}
	return nil, false
}
func (f *fakeConfig) GetStringSlice2D(key string) ([][]string, bool) {
	return nil, false
}

// testAccount is the bridge account used in tests. bridge.Bridge resolves
// config keys as <account>.<key>, so override keys are prefixed with it.
const testAccount = "irc.test"

func newTestBirc(overrides map[string]interface{}) *Birc {
	logger := logrus.New()
	logger.SetLevel(logrus.PanicLevel)
	prefixed := make(map[string]interface{}, len(overrides))
	for k, v := range overrides {
		prefixed[testAccount+"."+k] = v
	}
	return &Birc{
		Nick:                   "testbot",
		names:                  make(map[string][]string),
		connected:              make(chan error),
		channels:               make(map[string]bool),
		dmChannels:             make(map[string]bool),
		nickServRegCache:       make(map[string]time.Time),
		nickServLastChecked:    make(map[string]time.Time),
		nickServIgnoreDecision: make(map[string]bool),
		nickServActiveQueries:  make(map[string]bool),
		nickServRetries:        make(map[string]int),
		pendingMessages:        make(map[string][]pendingMsg),
		Config: &bridge.Config{
			Bridge: &bridge.Bridge{
				Account: testAccount,
				Log:     logrus.NewEntry(logger),
				Config:  &fakeConfig{overrides: prefixed},
			},
			Remote: make(chan config.Message, 16),
		},
	}
}

func privEvent(nick, channel, text string) girc.Event {
	return girc.Event{
		Command: "PRIVMSG",
		Source:  &girc.Source{Name: nick, Ident: "user", Host: "host"},
		Params:  []string{channel, text},
	}
}

func drainRemote(b *Birc) []config.Message {
	var out []config.Message
	for {
		select {
		case m := <-b.Remote:
			out = append(out, m)
		default:
			return out
		}
	}
}

func waitForRemote(t *testing.T, b *Birc, timeout time.Duration) []config.Message {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case m := <-b.Remote:
			out := []config.Message{m}
			out = append(out, drainRemote(b)...)
			return out
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	return nil
}

// waitFor polls cond every 10ms until it returns true or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.FailNowf(t, "condition not met within timeout", "timeout after %v", timeout)
}

// newFakeGircClient builds a girc client that is NOT connected to a server.
// Outbound messages (NICKSERV INFO) are safely no-oped because the client has
// no connection, while inbound handlers (AddTmp for NOTICE) still work and can
// be driven manually via RunHandlers. This lets us exercise
// fetchNickServRegistration end-to-end without a network.
func newFakeGircClient(t *testing.T, b *Birc) *girc.Client {
	t.Helper()
	c := girc.New(girc.Config{
		Nick:      b.Nick,
		User:      "test",
		Name:      "test",
		PingDelay: -1,
		RecoverFunc: func(cl *girc.Client, e *girc.HandlerError) {
			t.Logf("recovered handler panic: %v", e.Panic)
		},
	})
	b.i = c
	return c
}

// ---------------------------------------------------------------------------
// evaluateRecentlyRegistered — the decision matrix
// ---------------------------------------------------------------------------

func TestEvaluateRecentlyRegistered(t *testing.T) {
	ignoreDays := 30

	t.Run("cached recent decision is reused", func(t *testing.T) {
		b := newTestBirc(nil)
		b.nickServRegCache["acc"] = time.Now().Add(-100 * 24 * time.Hour)
		b.nickServLastChecked["acc"] = time.Now().Add(-1 * time.Hour)
		b.nickServIgnoreDecision["acc"] = false

		d := b.evaluateRecentlyRegistered("acc", ignoreDays)
		assert.False(t, d.hold)
		assert.False(t, d.gaveUp)
		assert.False(t, d.shouldIgnore)
	})

	t.Run("cached recent ignore decision is reused", func(t *testing.T) {
		b := newTestBirc(nil)
		b.nickServRegCache["acc"] = time.Now().Add(-1 * 24 * time.Hour)
		b.nickServLastChecked["acc"] = time.Now().Add(-1 * time.Hour)
		b.nickServIgnoreDecision["acc"] = true

		d := b.evaluateRecentlyRegistered("acc", ignoreDays)
		assert.False(t, d.hold)
		assert.False(t, d.gaveUp)
		assert.True(t, d.shouldIgnore)
	})

	t.Run("cached but stale is re-evaluated from known date", func(t *testing.T) {
		b := newTestBirc(nil)
		// registered 40 days ago, checked 2 days ago (window expired)
		b.nickServRegCache["acc"] = time.Now().Add(-40 * 24 * time.Hour)
		b.nickServLastChecked["acc"] = time.Now().Add(-2 * 24 * time.Hour)
		// stale decision says ignore, but the account is old enough now
		b.nickServIgnoreDecision["acc"] = true

		d := b.evaluateRecentlyRegistered("acc", ignoreDays)
		assert.False(t, d.hold)
		assert.False(t, d.gaveUp)
		assert.False(t, d.shouldIgnore, "40 days > 30 → should no longer be ignored")
		assert.False(t, b.nickServIgnoreDecision["acc"], "decision should be refreshed")
	})

	t.Run("cached but stale and still young is ignored", func(t *testing.T) {
		b := newTestBirc(nil)
		b.nickServRegCache["acc"] = time.Now().Add(-5 * 24 * time.Hour)
		b.nickServLastChecked["acc"] = time.Now().Add(-2 * 24 * time.Hour)

		d := b.evaluateRecentlyRegistered("acc", ignoreDays)
		assert.False(t, d.hold)
		assert.False(t, d.gaveUp)
		assert.True(t, d.shouldIgnore, "5 days < 30 → ignore")
	})

	t.Run("active query holds without firing another", func(t *testing.T) {
		b := newTestBirc(nil)
		b.nickServActiveQueries["acc"] = true

		d := b.evaluateRecentlyRegistered("acc", ignoreDays)
		assert.True(t, d.hold)
		assert.False(t, d.fire)
		assert.False(t, d.gaveUp)
	})

	t.Run("unknown account fires first attempt and holds", func(t *testing.T) {
		b := newTestBirc(nil)

		d := b.evaluateRecentlyRegistered("acc", ignoreDays)
		assert.True(t, d.hold)
		assert.True(t, d.fire)
		assert.Equal(t, 0, d.retry, "first attempt has no delay")
		assert.Equal(t, 1, b.nickServRetries["acc"])
	})

	t.Run("failed first attempt schedules second with delay", func(t *testing.T) {
		b := newTestBirc(nil)
		b.nickServRetries["acc"] = 1
		b.nickServLastChecked["acc"] = time.Now().Add(-1 * time.Minute)

		d := b.evaluateRecentlyRegistered("acc", ignoreDays)
		assert.True(t, d.hold)
		assert.True(t, d.fire)
		assert.Equal(t, 1, d.retry)
		assert.Equal(t, 2, b.nickServRetries["acc"])
	})

	t.Run("failed second attempt schedules third with delay", func(t *testing.T) {
		b := newTestBirc(nil)
		b.nickServRetries["acc"] = 2
		b.nickServLastChecked["acc"] = time.Now().Add(-1 * time.Minute)

		d := b.evaluateRecentlyRegistered("acc", ignoreDays)
		assert.True(t, d.hold)
		assert.True(t, d.fire)
		assert.Equal(t, 2, d.retry)
		assert.Equal(t, 3, b.nickServRetries["acc"])
	})

	t.Run("gave up within 24h allows through", func(t *testing.T) {
		b := newTestBirc(nil)
		b.nickServRetries["acc"] = 3
		b.nickServLastChecked["acc"] = time.Now().Add(-1 * time.Hour)

		d := b.evaluateRecentlyRegistered("acc", ignoreDays)
		assert.False(t, d.hold)
		assert.False(t, d.fire)
		assert.True(t, d.gaveUp)
		assert.False(t, d.shouldIgnore)
	})

	t.Run("gave up but 24h expired restarts verification", func(t *testing.T) {
		b := newTestBirc(nil)
		b.nickServRetries["acc"] = 3
		b.nickServLastChecked["acc"] = time.Now().Add(-2 * 24 * time.Hour)

		d := b.evaluateRecentlyRegistered("acc", ignoreDays)
		assert.True(t, d.hold)
		assert.True(t, d.fire)
		assert.Equal(t, 0, d.retry, "restarts at the first delay slot")
		assert.Equal(t, 1, b.nickServRetries["acc"], "retry counter is reset")
		assert.False(t, d.gaveUp)
	})
}

// ---------------------------------------------------------------------------
// skipRecentlyRegistered — hold / ignore / allow glue
// ---------------------------------------------------------------------------

func TestSkipRecentlyRegistered_HoldsMessage(t *testing.T) {
	b := newTestBirc(nil)
	// Simulate a query already in flight: hold, do not fire, do not drop.
	b.nickServActiveQueries["acc"] = true

	got := b.skipRecentlyRegistered("acc", privEvent("nick1", "#chan", "hello"), 30)

	require.True(t, got, "message should be held (not forwarded directly)")
	b.pendingMessagesMu.Lock()
	pending := b.pendingMessages["acc"]
	b.pendingMessagesMu.Unlock()
	require.Len(t, pending, 1)
	assert.Equal(t, "hello", pending[0].Msg.Text)
	assert.Equal(t, "#chan", pending[0].Msg.Channel)
	assert.Equal(t, "nick1", pending[0].Msg.Username)
	assert.WithinDuration(t, time.Now(), pending[0].QueuedAt, time.Minute)
}

func TestSkipRecentlyRegistered_IgnoresRecentlyRegistered(t *testing.T) {
	b := newTestBirc(nil)
	b.nickServRegCache["acc"] = time.Now().Add(-2 * 24 * time.Hour)
	b.nickServLastChecked["acc"] = time.Now().Add(-1 * time.Hour)
	b.nickServIgnoreDecision["acc"] = true

	got := b.skipRecentlyRegistered("acc", privEvent("nick1", "#chan", "hello"), 30)

	assert.True(t, got)
	assert.Empty(t, drainRemote(b))
	b.pendingMessagesMu.Lock()
	_, exists := b.pendingMessages["acc"]
	b.pendingMessagesMu.Unlock()
	assert.False(t, exists, "ignored messages must not be queued")
}

func TestSkipRecentlyRegistered_AllowsOldAccount(t *testing.T) {
	b := newTestBirc(nil)
	b.nickServRegCache["acc"] = time.Now().Add(-100 * 24 * time.Hour)
	b.nickServLastChecked["acc"] = time.Now().Add(-1 * time.Hour)
	b.nickServIgnoreDecision["acc"] = false

	got := b.skipRecentlyRegistered("acc", privEvent("nick1", "#chan", "hello"), 30)

	assert.False(t, got)
}

// ---------------------------------------------------------------------------
// processPendingMessages — flush, bounded hold, ignore decision
// ---------------------------------------------------------------------------

func TestProcessPendingMessages_ForwardsOldAccount(t *testing.T) {
	b := newTestBirc(map[string]interface{}{
		"IgnoreRegistered":     []string{"*"},
		"IgnoreRegisteredDays": 30,
	})
	b.nickServRegCache["acc"] = time.Now().Add(-100 * 24 * time.Hour)

	b.pendingMessagesMu.Lock()
	b.pendingMessages["acc"] = []pendingMsg{
		{Msg: config.Message{Username: "nick1", Channel: "#chan", Text: "msg1"}, QueuedAt: time.Now()},
		{Msg: config.Message{Username: "nick1", Channel: "#chan", Text: "msg2"}, QueuedAt: time.Now()},
	}
	b.pendingMessagesMu.Unlock()

	b.processPendingMessages("acc")

	msgs := waitForRemote(t, b, 2*time.Second)
	require.Len(t, msgs, 2)
	assert.Equal(t, "msg1", msgs[0].Text)
	assert.Equal(t, "msg2", msgs[1].Text)

	b.pendingMessagesMu.Lock()
	_, exists := b.pendingMessages["acc"]
	b.pendingMessagesMu.Unlock()
	assert.False(t, exists, "queue must be cleared after flush")
}

func TestProcessPendingMessages_DropsYoungAccount(t *testing.T) {
	b := newTestBirc(map[string]interface{}{
		"IgnoreRegistered":     []string{"*"},
		"IgnoreRegisteredDays": 30,
	})
	b.nickServRegCache["acc"] = time.Now().Add(-2 * 24 * time.Hour) // young

	b.pendingMessagesMu.Lock()
	b.pendingMessages["acc"] = []pendingMsg{
		{Msg: config.Message{Username: "nick1", Channel: "#chan", Text: "msg1"}, QueuedAt: time.Now()},
	}
	b.pendingMessagesMu.Unlock()

	b.processPendingMessages("acc")

	time.Sleep(100 * time.Millisecond)
	assert.Empty(t, drainRemote(b), "young account's held messages must be dropped")
}

func TestProcessPendingMessages_ForwardsWhenNoRegDate(t *testing.T) {
	b := newTestBirc(map[string]interface{}{
		"IgnoreRegistered":     []string{"*"},
		"IgnoreRegisteredDays": 30,
	})
	// No cached registration date (e.g. lookup gave up / unregistered) → forward

	b.pendingMessagesMu.Lock()
	b.pendingMessages["acc"] = []pendingMsg{
		{Msg: config.Message{Username: "nick1", Channel: "#chan", Text: "msg1"}, QueuedAt: time.Now()},
	}
	b.pendingMessagesMu.Unlock()

	b.processPendingMessages("acc")

	msgs := waitForRemote(t, b, 2*time.Second)
	require.Len(t, msgs, 1)
	assert.Equal(t, "msg1", msgs[0].Text)
}

func TestProcessPendingMessages_BoundedHoldDropsStale(t *testing.T) {
	b := newTestBirc(map[string]interface{}{
		"IgnoreRegistered":     []string{"*"},
		"IgnoreRegisteredDays": 30,
	})
	b.nickServRegCache["acc"] = time.Now().Add(-100 * 24 * time.Hour) // old, would be forwarded

	b.pendingMessagesMu.Lock()
	b.pendingMessages["acc"] = []pendingMsg{
		{Msg: config.Message{Username: "nick1", Channel: "#chan", Text: "stale"}, QueuedAt: time.Now().Add(-20 * time.Second)},
		{Msg: config.Message{Username: "nick1", Channel: "#chan", Text: "fresh"}, QueuedAt: time.Now()},
	}
	b.pendingMessagesMu.Unlock()

	b.processPendingMessages("acc")

	msgs := waitForRemote(t, b, 2*time.Second)
	require.Len(t, msgs, 1, "only the fresh message should survive the 15s bounded hold")
	assert.Equal(t, "fresh", msgs[0].Text)
}

func TestProcessPendingMessages_OnlyMatchesConfiguredChannels(t *testing.T) {
	b := newTestBirc(map[string]interface{}{
		"IgnoreRegistered":     []string{"#target"},
		"IgnoreRegisteredDays": 30,
	})
	b.nickServRegCache["acc"] = time.Now().Add(-2 * 24 * time.Hour) // young, ignored in #target

	b.pendingMessagesMu.Lock()
	b.pendingMessages["acc"] = []pendingMsg{
		{Msg: config.Message{Username: "nick1", Channel: "#other", Text: "other"}, QueuedAt: time.Now()},
		{Msg: config.Message{Username: "nick1", Channel: "#target", Text: "target"}, QueuedAt: time.Now()},
	}
	b.pendingMessagesMu.Unlock()

	b.processPendingMessages("acc")

	msgs := waitForRemote(t, b, 2*time.Second)
	require.Len(t, msgs, 1)
	assert.Equal(t, "#other", msgs[0].Channel, "message outside configured channels must be forwarded")
}

// ---------------------------------------------------------------------------
// queueMessageForAccount — event → message conversion
// ---------------------------------------------------------------------------

func TestQueueMessageForAccount_PlainMessage(t *testing.T) {
	b := newTestBirc(nil)
	b.queueMessageForAccount("acc", privEvent("nick1", "#chan", "hello world"))

	b.pendingMessagesMu.Lock()
	pending := b.pendingMessages["acc"]
	b.pendingMessagesMu.Unlock()
	require.Len(t, pending, 1)
	assert.Equal(t, "hello world", pending[0].Msg.Text)
	assert.Equal(t, "#chan", pending[0].Msg.Channel)
	assert.Equal(t, "nick1", pending[0].Msg.Username)
	assert.Equal(t, "user@host", pending[0].Msg.UserID)
	assert.Empty(t, pending[0].Msg.Event)
}

func TestQueueMessageForAccount_Action(t *testing.T) {
	b := newTestBirc(nil)
	// CTCP ACTION: \x01ACTION <text>\x01
	ev := girc.Event{
		Command: "PRIVMSG",
		Source:  &girc.Source{Name: "nick1", Ident: "user", Host: "host"},
		Params:  []string{"#chan", "\x01ACTION does something\x01"},
	}
	b.queueMessageForAccount("acc", ev)

	b.pendingMessagesMu.Lock()
	pending := b.pendingMessages["acc"]
	b.pendingMessagesMu.Unlock()
	require.Len(t, pending, 1)
	assert.Equal(t, "does something", pending[0].Msg.Text, "ACTION text should be stripped")
	assert.Equal(t, config.EventUserAction, pending[0].Msg.Event)
}

func TestQueueMessageForAccount_Notice(t *testing.T) {
	b := newTestBirc(nil)
	ev := girc.Event{
		Command: "NOTICE",
		Source:  &girc.Source{Name: "nick1", Ident: "user", Host: "host"},
		Params:  []string{"#chan", "a notice"},
	}
	b.queueMessageForAccount("acc", ev)

	b.pendingMessagesMu.Lock()
	pending := b.pendingMessages["acc"]
	b.pendingMessagesMu.Unlock()
	require.Len(t, pending, 1)
	assert.Equal(t, "a notice", pending[0].Msg.Text)
	assert.Equal(t, config.EventNoticeIRC, pending[0].Msg.Event)
}

// ---------------------------------------------------------------------------
// parseNickServDate
// ---------------------------------------------------------------------------

func TestParseNickServDate(t *testing.T) {
	testcases := map[string]struct {
		input    string
		wantYear int
		wantErr  bool
	}{
		"atime named tz":       {"Registered : Jan 02 00:00:00 2020 UTC", 2020, false},
		"atime numeric offset": {"Registered : Jul 03 18:30:15 2021 +0000 (4y 36w 3d ago)", 2021, false},
		"atime no tz":          {"Registered : Mar 15 12:30:00 2023", 2023, false},
		"anope format":         {"Registered: Mon Jan 02 00:00:00 2017", 2017, false},
		"unix timestamp":       {"Time registered : 1577836800", 2020, false},
		"no colon":             {"Registered Jan 02 2020", 0, true},
		"garbage date":         {"Registered : not a date at all", 0, true},
	}

	for name, tc := range testcases {
		t.Run(name, func(t *testing.T) {
			got, err := parseNickServDate(tc.input)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantYear, got.Year())
		})
	}
}

// ---------------------------------------------------------------------------
// NickServ line parsing & result handling
// ---------------------------------------------------------------------------

func TestClassifyNickServLine(t *testing.T) {
	testcases := map[string]struct {
		input     string
		wantFound bool
		wantTerm  bool
		wantYear  int
	}{
		"atime registered line":      {"Registered : Jan 02 00:00:00 2020 UTC", true, false, 2020},
		"atime registered with age":  {"Registered : Jul 03 18:30:15 2021 +0000 (4y 36w 3d ago)", true, false, 2021},
		"anope registered line":      {"Registered: Mon Jan 02 00:00:00 2017", true, false, 2017},
		"unix timestamp line":        {"Time registered : 1577836800", true, false, 2020},
		"end of info":                {"*** End of Info ***", false, true, 0},
		"not registered":             {"acc isn't registered.", false, true, 0},
		"other info line":            {"Account: acc1", false, false, 0},
		"unparseable registered line": {"Registered : not a date", false, false, 0},
	}

	for name, tc := range testcases {
		t.Run(name, func(t *testing.T) {
			line := classifyNickServLine(tc.input)
			assert.Equal(t, tc.wantFound, line.found)
			assert.Equal(t, tc.wantTerm, line.term)
			if tc.wantFound {
				assert.Equal(t, tc.wantYear, line.regDate.Year())
			}
		})
	}
}

func TestAccumulateNickServLines(t *testing.T) {
	old := time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC)

	t.Run("first found line wins", func(t *testing.T) {
		reg, found := accumulateNickServLines([]nickservLine{
			{found: true, regDate: old},
			{},
			{found: true, regDate: time.Date(2022, 5, 6, 0, 0, 0, 0, time.UTC)},
			{term: true},
		})
		assert.True(t, found)
		assert.True(t, old.Equal(reg))
	})

	t.Run("no found line", func(t *testing.T) {
		_, found := accumulateNickServLines([]nickservLine{{term: true}})
		assert.False(t, found)
	})

	t.Run("empty", func(t *testing.T) {
		_, found := accumulateNickServLines(nil)
		assert.False(t, found)
	})
}

// ---------------------------------------------------------------------------
// fetchNickServRegistration — real transport, driven synchronously
// ---------------------------------------------------------------------------

// TestFetchNickServRegistration_TimeoutFlushesPending drives the real
// fetchNickServRegistration (unconnected client → NICKSERV INFO is a no-op →
// the AddTmp handler never fires) and verifies the 2s timeout path flushes
// the held messages as forwarded (no registration date known).
func TestFetchNickServRegistration_TimeoutFlushesPending(t *testing.T) {
	b := newTestBirc(map[string]interface{}{
		"IgnoreRegistered":     []string{"*"},
		"IgnoreRegisteredDays": 30,
	})
	newFakeGircClient(t, b)

	b.queueMessageForAccount("acc", privEvent("nick1", "#chan", "held message"))

	start := time.Now()
	b.fetchNickServRegistration("acc") // synchronous: blocks ~2s on the timeout
	elapsed := time.Since(start)

	assert.GreaterOrEqual(t, elapsed, 2*time.Second, "should wait for the NickServ deadline")
	assert.Less(t, elapsed, 5*time.Second, "should not exceed the deadline by much")

	// Pending messages are flushed; no reg date → forwarded
	msgs := waitForRemote(t, b, time.Second)
	require.Len(t, msgs, 1)
	assert.Equal(t, "held message", msgs[0].Text)

	// No registration date cached, active query cleared
	b.nickServCacheMu.Lock()
	_, cached := b.nickServRegCache["acc"]
	active := b.nickServActiveQueries["acc"]
	b.nickServCacheMu.Unlock()
	assert.False(t, cached)
	assert.False(t, active)
}

// TestFinishNickServRegistration_ApplyResult covers the success path without
// the network: applying a found registration date updates the cache and
// ignore decision and drops the held messages of a young account.
func TestFinishNickServRegistration_ApplyResult(t *testing.T) {
	b := newTestBirc(map[string]interface{}{
		"IgnoreRegistered":     []string{"*"},
		"IgnoreRegisteredDays": 30,
	})

	b.queueMessageForAccount("acc", privEvent("nick1", "#chan", "held message"))

	regDate := time.Now().Add(-2 * 24 * time.Hour) // young
	b.finishNickServRegistration("acc", regDate, true)

	time.Sleep(100 * time.Millisecond)
	assert.Empty(t, drainRemote(b), "young account's held message must be dropped")

	b.nickServCacheMu.Lock()
	cachedDate, cached := b.nickServRegCache["acc"]
	decision := b.nickServIgnoreDecision["acc"]
	b.nickServCacheMu.Unlock()
	require.True(t, cached)
	assert.True(t, regDate.Equal(cachedDate))
	assert.True(t, decision, "young account should be marked to ignore")
}

// TestFinishNickServRegistration_OldAccountFlushes covers the success path for
// an old account: held messages are forwarded.
func TestFinishNickServRegistration_OldAccountFlushes(t *testing.T) {
	b := newTestBirc(map[string]interface{}{
		"IgnoreRegistered":     []string{"*"},
		"IgnoreRegisteredDays": 30,
	})

	b.queueMessageForAccount("acc", privEvent("nick1", "#chan", "held message"))

	regDate := time.Now().Add(-100 * 24 * time.Hour) // old
	b.finishNickServRegistration("acc", regDate, true)

	msgs := waitForRemote(t, b, time.Second)
	require.Len(t, msgs, 1)
	assert.Equal(t, "held message", msgs[0].Text)

	b.nickServCacheMu.Lock()
	decision := b.nickServIgnoreDecision["acc"]
	b.nickServCacheMu.Unlock()
	assert.False(t, decision, "old account should not be marked to ignore")
}

// ---------------------------------------------------------------------------
// End-to-end: skipRecentlyRegistered hold → give-up → pass-through
// ---------------------------------------------------------------------------

// TestEndToEnd_HoldBurstThenTimeout covers the core behavior change: a burst
// of messages from an unknown account is held while the (timed-out) NickServ
// lookup runs, then flushed together. Uses the unconnected-client timeout
// path, which is deterministic (~2s).
func TestEndToEnd_HoldBurstThenTimeout(t *testing.T) {
	b := newTestBirc(map[string]interface{}{
		"IgnoreRegistered":     []string{"*"},
		"IgnoreRegisteredDays": 30,
	})
	newFakeGircClient(t, b)

	// 1. First message from unknown account → held, query fired
	got := b.skipRecentlyRegistered("acc", privEvent("newnick", "#chan", "first message"), 30)
	require.True(t, got, "first message must be held")

	// 2. A burst arrives while the query is in flight → also held (not passed through)
	got = b.skipRecentlyRegistered("acc", privEvent("newnick", "#chan", "second message"), 30)
	require.True(t, got, "burst message while query in flight must be held")
	got = b.skipRecentlyRegistered("acc", privEvent("newnick", "#chan", "third message"), 30)
	require.True(t, got)

	// 3. Wait for the query to complete (2s timeout, no NickServ response)
	waitFor(t, 5*time.Second, func() bool {
		b.nickServCacheMu.Lock()
		defer b.nickServCacheMu.Unlock()
		return !b.nickServActiveQueries["acc"]
	})

	// 4. The held burst was flushed as forwarded (no registration date known)
	msgs := waitForRemote(t, b, 2*time.Second)
	require.Len(t, msgs, 3, "all held messages should be flushed after the lookup completes")
	assert.Equal(t, "first message", msgs[0].Text, "FIFO order must be preserved")
	assert.Equal(t, "second message", msgs[1].Text)
	assert.Equal(t, "third message", msgs[2].Text)

	// 5. A later message schedules the retry (attempt 2, 2s delay) and is held
	got = b.skipRecentlyRegistered("acc", privEvent("newnick", "#chan", "fourth message"), 30)
	assert.True(t, got, "second attempt must hold and re-query")
	assert.Equal(t, 2, b.nickServRetries["acc"])
}

// TestEndToEnd_BoundedHoldDropsStaleMessages verifies that when the lookup
// takes longer than the 15s bounded hold, stale held messages are dropped
// while the (late) fresh ones are still forwarded.
func TestEndToEnd_BoundedHoldDropsStaleMessages(t *testing.T) {
	b := newTestBirc(map[string]interface{}{
		"IgnoreRegistered":     []string{"*"},
		"IgnoreRegisteredDays": 30,
	})
	newFakeGircClient(t, b)

	// Hold a first message (fires the query, ~2s timeout)
	got := b.skipRecentlyRegistered("acc", privEvent("newnick", "#chan", "early message"), 30)
	require.True(t, got)

	// Simulate a message that has been held longer than the bounded hold
	// (e.g. a stuck lookup on a real network).
	b.pendingMessagesMu.Lock()
	b.pendingMessages["acc"] = append(b.pendingMessages["acc"], pendingMsg{
		Msg:      config.Message{Username: "newnick", Channel: "#chan", Text: "stale message"},
		QueuedAt: time.Now().Add(-20 * time.Second),
	})
	b.pendingMessagesMu.Unlock()

	// Wait for the lookup to time out and flush
	waitFor(t, 5*time.Second, func() bool {
		b.nickServCacheMu.Lock()
		defer b.nickServCacheMu.Unlock()
		return !b.nickServActiveQueries["acc"]
	})

	msgs := waitForRemote(t, b, 2*time.Second)
	require.Len(t, msgs, 1, "only the fresh message should survive the 15s bounded hold")
	assert.Equal(t, "early message", msgs[0].Text)
}
