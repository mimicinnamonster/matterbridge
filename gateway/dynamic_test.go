package gateway

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMapChannelName(t *testing.T) {
	r := &Router{}
	tests := []struct {
		srcAccount  string
		destAccount string
		channelName string
		expected    string
	}{
		{"discord.test", "irc.test", "channel", "channel"},
		{"discord.test", "irc.test", "_channel", "#channel"},
		{"discord.test", "irc.test", "__channel", "##channel"},
		{"irc.test", "discord.test", "#channel", "_channel"},
		{"irc.test", "discord.test", "##channel", "__channel"},
		{"irc.test", "discord.test", "###channel", "___channel"},
		{"slack.test", "discord.test", "general", "general"},
	}

	for _, tt := range tests {
		actual := r.mapChannelName(tt.srcAccount, tt.destAccount, tt.channelName)
		assert.Equal(t, tt.expected, actual, "mapping %s to %s for channel %s", tt.srcAccount, tt.destAccount, tt.channelName)
	}
}

func TestBDiscordToIRC(t *testing.T) {
	assert.Equal(t, "channel", bDiscordToIRC("channel"))
	assert.Equal(t, "#channel", bDiscordToIRC("_channel"))
	assert.Equal(t, "##channel", bDiscordToIRC("__channel"))
	assert.Equal(t, "###channel", bDiscordToIRC("___channel"))
	assert.Equal(t, "###channel_with_underscores", bDiscordToIRC("___channel_with_underscores"))
}

func TestBIRCToDiscord(t *testing.T) {
	assert.Equal(t, "channel", bIRCToDiscord("channel"))
	assert.Equal(t, "_channel", bIRCToDiscord("#channel"))
	assert.Equal(t, "__channel", bIRCToDiscord("##channel"))
	assert.Equal(t, "___channel", bIRCToDiscord("###channel"))
}
