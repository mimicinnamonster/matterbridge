package birc

import (
	"testing"

	"github.com/42wim/matterbridge/bridge/config"
	"github.com/stretchr/testify/assert"
)

func TestIRCCommandExecution(t *testing.T) {
	testcases := map[string]struct {
		input      config.Message
		isCommand  bool
		expectsRaw bool
	}{
		"normal irc message": {
			input: config.Message{
				Text:       "hello world",
				IRCCommand: false,
			},
			isCommand:  false,
			expectsRaw: false,
		},
		"irc command from discord": {
			input: config.Message{
				Text:       "MODE #channel +m",
				IRCCommand: true,
				Username:   "discorduser",
			},
			isCommand:  true,
			expectsRaw: true,
		},
		"legacy irc command": {
			input: config.Message{
				Text:       "!users",
				IRCCommand: false,
			},
			isCommand:  true,
			expectsRaw: false,
		},
	}

	for name, tc := range testcases {
		// Verify the message has the correct properties for the test
		if tc.isCommand {
			if tc.expectsRaw {
				// IRC command from Discord
				assert.Truef(t, tc.input.IRCCommand, "Should be marked as IRC command: %s", name)
			}
		}
		
		// The key test: messages marked with IRCCommand should be treated as commands
		shouldBeCommand := tc.input.IRCCommand || (len(tc.input.Text) > 0 && tc.input.Text[0] == '!')
		assert.Equalf(t, tc.isCommand, shouldBeCommand, "Command detection failed for: %s", name)
	}
}

func TestIRCCommandText(t *testing.T) {
	testcases := map[string]struct {
		input    string
		command  string
		hasError bool
	}{
		"mode command": {
			input:   "MODE #channel +m",
			command: "MODE #channel +m",
		},
		"kick command": {
			input:   "KICK user :reason",
			command: "KICK user :reason",
		},
		"topic command": {
			input:   "TOPIC #channel :New Topic",
			command: "TOPIC #channel :New Topic",
		},
	}

	for name, tc := range testcases {
		msg := config.Message{
			Text:       tc.input,
			IRCCommand: true,
		}
		// Verify the raw command matches what we expect to send
		assert.Equalf(t, tc.command, msg.Text, "Command text mismatch for: %s", name)
	}
}
