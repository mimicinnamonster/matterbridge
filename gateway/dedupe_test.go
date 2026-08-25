package gateway

import (
	"fmt"
	"io/ioutil"
	"testing"
	"time"

	"github.com/42wim/matterbridge/bridge/config"
	"github.com/42wim/matterbridge/gateway/bridgemap"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
)

// makeDedupeRouter returns a Router with a given dedupe window in seconds.
func makeDedupeRouter(seconds int) *Router {
	logger := logrus.New()
	logger.SetOutput(ioutil.Discard)
	cfg := config.NewConfigFromString(logger, []byte(fmt.Sprintf("[general]\nDedupeSeconds = %d\n", seconds)))
	r, err := NewRouter(logger, cfg, bridgemap.FullMap)
	if err != nil {
		panic(err)
	}
	return r
}

func ircMsg(text, channel, username string) *config.Message {
	return &config.Message{
		Text:     text,
		Channel:  channel,
		Username: username,
		Account:  "irc.znc",
	}
}

func TestIsDuplicateMessage(t *testing.T) {
	r := makeDedupeRouter(5)
	msg := ircMsg("hello", "##programming", "bob")

	assert.False(t, r.isDuplicateMessage(msg), "first occurrence must not be a duplicate")
	assert.True(t, r.isDuplicateMessage(msg), "immediate repeat within window must be a duplicate")
	assert.True(t, r.isDuplicateMessage(msg), "repeat without timestamp refresh stays a duplicate")

	// a different message is not a duplicate
	other := ircMsg("hello world", "##programming", "bob")
	assert.False(t, r.isDuplicateMessage(other), "different text must not be a duplicate")

	// different channel is not a duplicate
	other = ircMsg("hello", "#ai", "bob")
	assert.False(t, r.isDuplicateMessage(other), "different channel must not be a duplicate")

	// different user is not a duplicate
	other = ircMsg("hello", "##programming", "alice")
	assert.False(t, r.isDuplicateMessage(other), "different username must not be a duplicate")

	// different bridge is not a duplicate
	other = ircMsg("hello", "##programming", "bob")
	other.Account = "irc.libera"
	assert.False(t, r.isDuplicateMessage(other), "different account must not be a duplicate")
}

func TestIsDuplicateMessageWindowExpiry(t *testing.T) {
	r := makeDedupeRouter(1)
	msg := ircMsg("again", "##programming", "bob")

	assert.False(t, r.isDuplicateMessage(msg))
	assert.True(t, r.isDuplicateMessage(msg))

	// after the window expires the same message is no longer a duplicate
	time.Sleep(1100 * time.Millisecond)
	assert.False(t, r.isDuplicateMessage(msg), "message after window expiry must pass")
	assert.True(t, r.isDuplicateMessage(msg), "and its immediate repeat is caught again")
}

func TestIsDuplicateMessageDisabled(t *testing.T) {
	r := makeDedupeRouter(0)
	msg := ircMsg("hello", "##programming", "bob")

	assert.False(t, r.isDuplicateMessage(msg))
	assert.False(t, r.isDuplicateMessage(msg), "zero window disables deduplication")
}
