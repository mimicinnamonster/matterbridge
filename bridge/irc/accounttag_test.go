package birc

import (
	"testing"
	"time"

	"github.com/lrstanley/girc"
	"github.com/stretchr/testify/assert"
)

// ---------------------------------------------------------------------------
// accountFromTag — IRCv3 account-tag interpretation
// ---------------------------------------------------------------------------

// Per the IRCv3 account-tag spec, users who are not logged in to services
// still carry the account tag with the value "*", so tag presence alone is
// not proof of registration.
func TestAccountFromTag(t *testing.T) {
	testcases := map[string]struct {
		tags             girc.Tags
		wantAccount      string
		wantRegistration bool
	}{
		"nil tags": {
			tags:             nil,
			wantAccount:      "",
			wantRegistration: false,
		},
		"no account tag": {
			tags:             girc.Tags{},
			wantAccount:      "",
			wantRegistration: false,
		},
		"account=* (not logged in)": {
			tags:             girc.Tags{"account": "*"},
			wantAccount:      "",
			wantRegistration: false,
		},
		"empty account value": {
			tags:             girc.Tags{"account": ""},
			wantAccount:      "",
			wantRegistration: false,
		},
		"named account": {
			tags:             girc.Tags{"account": "alice"},
			wantAccount:      "alice",
			wantRegistration: true,
		},
	}

	for name, tc := range testcases {
		t.Run(name, func(t *testing.T) {
			account, registered := accountFromTag(tc.tags)
			assert.Equalf(t, tc.wantRegistration, registered, "registered mismatch for %q", name)
			assert.Equalf(t, tc.wantAccount, account, "account mismatch for %q", name)
		})
	}
}

// ---------------------------------------------------------------------------
// skipByAccountTag — IgnoreUnregistered
// ---------------------------------------------------------------------------

func TestSkipByAccountTagIgnoreUnregistered(t *testing.T) {
	testcases := map[string]struct {
		ignoreChannels []string
		eventChannel   string
		hasAccountTag  bool
		tags           girc.Tags
		wantSkip       bool
	}{
		"unconfigured channel passes": {
			ignoreChannels: []string{"##programming"},
			eventChannel:   "##unrelated",
			hasAccountTag:  true,
			tags:           girc.Tags{"account": "*"},
			wantSkip:       false,
		},
		"capability off passes": {
			ignoreChannels: []string{"##programming"},
			eventChannel:   "##programming",
			hasAccountTag:  false,
			tags:           girc.Tags{"account": "*"},
			wantSkip:       false,
		},
		"no account tag is ignored": {
			ignoreChannels: []string{"##programming"},
			eventChannel:   "##programming",
			hasAccountTag:  true,
			tags:           nil,
			wantSkip:       true,
		},
		"account=* is ignored": {
			ignoreChannels: []string{"##programming"},
			eventChannel:   "##programming",
			hasAccountTag:  true,
			tags:           girc.Tags{"account": "*"},
			wantSkip:       true,
		},
		"empty account value is ignored": {
			ignoreChannels: []string{"##programming"},
			eventChannel:   "##programming",
			hasAccountTag:  true,
			tags:           girc.Tags{"account": ""},
			wantSkip:       true,
		},
		"named account passes": {
			ignoreChannels: []string{"##programming"},
			eventChannel:   "##programming",
			hasAccountTag:  true,
			tags:           girc.Tags{"account": "alice"},
			wantSkip:       false,
		},
		"wildcard channel config ignores account=*": {
			ignoreChannels: []string{"*"},
			eventChannel:   "##programming",
			hasAccountTag:  true,
			tags:           girc.Tags{"account": "*"},
			wantSkip:       true,
		},
		"channel match is case-insensitive": {
			ignoreChannels: []string{"##Programming"},
			eventChannel:   "##programming",
			hasAccountTag:  true,
			tags:           girc.Tags{"account": "*"},
			wantSkip:       true,
		},
	}

	for name, tc := range testcases {
		t.Run(name, func(t *testing.T) {
			b := newTestBirc(map[string]interface{}{
				"IgnoreUnregistered": tc.ignoreChannels,
			})
			event := privEvent("sailorboomer", tc.eventChannel, "hello")
			event.Tags = tc.tags

			assert.Equalf(t, tc.wantSkip, b.skipByAccountTag(event, tc.hasAccountTag), "skip decision mismatch for %q", name)
		})
	}
}

// ---------------------------------------------------------------------------
// skipByAccountTag — IgnoreRegistered
// ---------------------------------------------------------------------------

func TestSkipByAccountTagIgnoreRegistered(t *testing.T) {
	testcases := map[string]struct {
		setup           func(b *Birc)
		ignoreChannels  []string
		eventChannel    string
		hasAccountTag   bool
		tags            girc.Tags
		wantSkip        bool
		wantNoNickServ  string // account that must have no NickServ retry state
	}{
		"unconfigured channel passes": {
			setup:          func(b *Birc) {},
			ignoreChannels: []string{"##programming"},
			eventChannel:   "##unrelated",
			hasAccountTag:  true,
			tags:           girc.Tags{"account": "newbie"},
			wantSkip:       false,
			wantNoNickServ: "newbie",
		},
		"capability off passes": {
			setup:          func(b *Birc) {},
			ignoreChannels: []string{"##programming"},
			eventChannel:   "##programming",
			hasAccountTag:  false,
			tags:           girc.Tags{"account": "newbie"},
			wantSkip:       false,
		},
		"recently registered named account is ignored": {
			setup: func(b *Birc) {
				b.nickServRegCache["newbie"] = time.Now().Add(-24 * time.Hour)
				b.nickServLastChecked["newbie"] = time.Now().Add(-time.Hour)
				b.nickServIgnoreDecision["newbie"] = true
			},
			ignoreChannels: []string{"##programming"},
			eventChannel:   "##programming",
			hasAccountTag:  true,
			tags:           girc.Tags{"account": "newbie"},
			wantSkip:       true,
		},
		"old named account passes": {
			setup: func(b *Birc) {
				b.nickServRegCache["veteran"] = time.Now().Add(-100 * 24 * time.Hour)
				b.nickServLastChecked["veteran"] = time.Now().Add(-time.Hour)
				b.nickServIgnoreDecision["veteran"] = false
			},
			ignoreChannels: []string{"##programming"},
			eventChannel:   "##programming",
			hasAccountTag:  true,
			tags:           girc.Tags{"account": "veteran"},
			wantSkip:       false,
		},
		"account=* passes and stays out of NickServ state": {
			setup:          func(b *Birc) {},
			ignoreChannels: []string{"##programming"},
			eventChannel:   "##programming",
			hasAccountTag:  true,
			tags:           girc.Tags{"account": "*"},
			wantSkip:       false,
			wantNoNickServ: "*",
		},
		"missing account tag passes and stays out of NickServ state": {
			setup:          func(b *Birc) {},
			ignoreChannels: []string{"##programming"},
			eventChannel:   "##programming",
			hasAccountTag:  true,
			tags:           nil,
			wantSkip:       false,
			wantNoNickServ: "*",
		},
	}

	for name, tc := range testcases {
		t.Run(name, func(t *testing.T) {
			b := newTestBirc(map[string]interface{}{
				"IgnoreRegistered":     tc.ignoreChannels,
				"IgnoreRegisteredDays": 30,
			})
			// Outbound NICKSERV queries are no-ops against this fake client;
			// it guards the goroutine in skipRecentlyRegistered against a nil b.i.
			newFakeGircClient(t, b)
			tc.setup(b)

			event := privEvent("someuser", tc.eventChannel, "hello")
			event.Tags = tc.tags

			assert.Equalf(t, tc.wantSkip, b.skipByAccountTag(event, tc.hasAccountTag), "skip decision mismatch for %q", name)

			if tc.wantNoNickServ != "" {
				_, tracked := b.nickServRetries[tc.wantNoNickServ]
				assert.Falsef(t, tracked, "account %q should not be tracked by NickServ verification", tc.wantNoNickServ)
			}
		})
	}
}

// Reproduces the production scenario: both filters are enabled for the same
// channel and an unlogged user (account=*) writes. The message must be
// dropped by IgnoreUnregistered, and "*" must never leak into the NickServ
// verification machinery.
func TestSkipByAccountTagProductionCombo(t *testing.T) {
	b := newTestBirc(map[string]interface{}{
		"IgnoreUnregistered":   []string{"##programming"},
		"IgnoreRegistered":     []string{"##programming"},
		"IgnoreRegisteredDays": 1460,
	})

	event := privEvent("sailorboomer", "##programming", "hello")
	event.Tags = girc.Tags{"account": "*"}

	assert.True(t, b.skipByAccountTag(event, true), "unlogged user should be ignored")
	_, tracked := b.nickServRetries["*"]
	assert.False(t, tracked, `"*" must not be tracked as a NickServ account`)
}
