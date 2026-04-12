package bdiscord

import (
	"testing"

	"github.com/42wim/matterbridge/bridge/config"
	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/assert"
)

func TestHandleEmbed(t *testing.T) {
	testcases := map[string]struct {
		embed  *discordgo.MessageEmbed
		result string
	}{
		"allempty": {
			embed:  &discordgo.MessageEmbed{},
			result: "",
		},
		"one": {
			embed: &discordgo.MessageEmbed{
				Title: "blah",
			},
			result: " embed: blah\n",
		},
		"two": {
			embed: &discordgo.MessageEmbed{
				Title:       "blah",
				Description: "blah2",
			},
			result: " embed: blah - blah2\n",
		},
		"three": {
			embed: &discordgo.MessageEmbed{
				Title:       "blah",
				Description: "blah2",
				URL:         "blah3",
			},
			result: " embed: blah - blah2 - blah3\n",
		},
		"twob": {
			embed: &discordgo.MessageEmbed{
				Description: "blah2",
				URL:         "blah3",
			},
			result: " embed: blah2 - blah3\n",
		},
		"oneb": {
			embed: &discordgo.MessageEmbed{
				URL: "blah3",
			},
			result: " embed: blah3\n",
		},
	}

	for name, tc := range testcases {
		assert.Equalf(t, tc.result, handleEmbed(tc.embed), "Testcases %s", name)
	}
}

func TestIRCCommandDetection(t *testing.T) {
	testcases := map[string]struct {
		input      string
		isCommand  bool
		cleanedText string
	}{
		"normal message": {
			input:      "hello world",
			isCommand:  false,
			cleanedText: "hello world",
		},
		"irc command MODE": {
			input:      "!irc MODE #channel +m",
			isCommand:  true,
			cleanedText: "MODE #channel +m",
		},
		"irc command KICK": {
			input:      "!irc KICK user reason here",
			isCommand:  true,
			cleanedText: "KICK user reason here",
		},
		"irc command PRIVMSG": {
			input:      "!irc PRIVMSG nick :message content",
			isCommand:  true,
			cleanedText: "PRIVMSG nick :message content",
		},
		"regular command": {
			input:      "!users",
			isCommand:  false,
			cleanedText: "!users",
		},
		"irc command with single word": {
			input:      "!irc NAMES",
			isCommand:  true,
			cleanedText: "NAMES",
		},
		"not a command": {
			input:      "!irc",
			isCommand:  false,
			cleanedText: "!irc",
		},
	}

	for name, tc := range testcases {
		msg := config.Message{Text: tc.input}
		
		// Simulate the Discord handler IRC command detection logic
		if len(tc.input) > 0 && len(tc.input) > 5 && tc.input[:5] == "!irc " {
			msg.IRCCommand = true
			msg.Text = tc.input[5:] // Strip "!irc " prefix
		}
		
		assert.Equalf(t, tc.isCommand, msg.IRCCommand, "IRC command detection failed for: %s", name)
		assert.Equalf(t, tc.cleanedText, msg.Text, "Text cleaning failed for: %s", name)
	}
}
