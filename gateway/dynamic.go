package gateway

import (
	"regexp"
	"strings"

	"github.com/42wim/matterbridge/bridge"
	"github.com/42wim/matterbridge/bridge/config"
)

var discordInvalidChars = regexp.MustCompile(`[^a-z0-9\-_]`)

// sanitizeForDiscord converts an IRC nick to a valid Discord channel name.
// Discord channel names must be lowercase, 2-100 chars, only [a-z0-9-_].
func sanitizeForDiscord(nick string) string {
	name := strings.ToLower(nick)
	name = discordInvalidChars.ReplaceAllString(name, "-")
	if len(name) < 2 {
		name = name + "--"
	}
	if len(name) > 100 {
		name = name[:100]
	}
	return name
}

func (r *Router) handleChannelCreate(msg *config.Message) {
	if msg.Event != config.EventChannelCreate {
		return
	}
	r.logger.Debugf("handleChannelCreate: %#v", msg)

	for _, gw := range r.Gateways {
		// find gateways with channel="*" for the source account
		var srcWildcard bool
		for _, channel := range gw.Channels {
			if channel.Account == msg.Account && channel.Name == "*" {
				srcWildcard = true
				break
			}
		}

		if !srcWildcard {
			continue
		}

		r.logger.Debugf("found wildcard gateway %s for source account %s", gw.Name, msg.Account)

		// find target bridges in the same gateway also having channel="*"
		for account, br := range gw.Bridges {
			if account == msg.Account {
				continue
			}

			// check if this bridge has wildcard channel configured
			var destWildcard bool
			for _, channel := range gw.Channels {
				if channel.Account == account && channel.Name == "*" {
					destWildcard = true
					break
				}
			}

			if !destWildcard {
				continue
			}

			r.logger.Debugf("found wildcard bridge %s in gateway %s", account, gw.Name)

			// create dynamic ChannelInfo for both sides
			srcChannelName := msg.Channel
			destChannelName := r.mapChannelName(msg.Account, account, srcChannelName)

			r.logger.Infof("dynamic bridging: %s (%s) <-> %s (%s) on gateway %s", msg.Account, srcChannelName, account, destChannelName, gw.Name)

			// add to gateway channels if not exists
			srcID := srcChannelName + msg.Account
			if _, ok := gw.Channels[srcID]; !ok {
				gw.Channels[srcID] = &config.ChannelInfo{
					Name:      srcChannelName,
					Account:   msg.Account,
					Direction: "inout",
					ID:        srcID,
					Options:   config.ChannelOptions{},
				}
				// add to bridge as well
				if srcBr, ok := gw.Bridges[msg.Account]; ok {
					srcBr.Channels[srcID] = *gw.Channels[srcID]
				}
			}

			destID := destChannelName + account
			if _, ok := gw.Channels[destID]; !ok {
				gw.Channels[destID] = &config.ChannelInfo{
					Name:      destChannelName,
					Account:   account,
					Direction: "inout",
					ID:        destID,
					Options:   config.ChannelOptions{},
				}
				// add to bridge as well
				br.Channels[destID] = *gw.Channels[destID]

				// join destination channel
				r.logger.Infof("joining dynamic channel %s on %s", destChannelName, account)
				err := br.JoinChannel(br.Channels[destID])
				if err != nil {
					r.logger.Errorf("failed to join dynamic channel %s on %s: %v", destChannelName, account, err)
				}
			}
		}
	}
}

func (r *Router) handleChannelDelete(msg *config.Message) {
	if msg.Event != config.EventChannelDelete {
		return
	}
	r.logger.Debugf("handleChannelDelete: %#v", msg)

	for _, gw := range r.Gateways {
		// find gateways with channel="*" for the source account
		var srcWildcard bool
		for _, channel := range gw.Channels {
			if channel.Account == msg.Account && channel.Name == "*" {
				srcWildcard = true
				break
			}
		}

		if !srcWildcard {
			continue
		}

		r.logger.Debugf("found wildcard gateway %s for source account %s", gw.Name, msg.Account)

		// find target bridges and part from them
		for account, br := range gw.Bridges {
			if account == msg.Account {
				continue
			}

			destChannelName := r.mapChannelName(msg.Account, account, msg.Channel)
			destID := destChannelName + account

			if channel, ok := gw.Channels[destID]; ok {
				r.logger.Infof("dynamic bridging: parting %s (%s) on gateway %s", account, destChannelName, gw.Name)
				err := br.PartChannel(*channel)
				if err != nil {
					r.logger.Errorf("failed to part dynamic channel %s on %s: %v", destChannelName, account, err)
				}
				delete(gw.Channels, destID)
				delete(br.Channels, destID)
			}
		}

		// remove source channel from gateway/bridge as well
		srcID := msg.Channel + msg.Account
		if _, ok := gw.Channels[srcID]; ok {
			delete(gw.Channels, srcID)
			if srcBr, ok := gw.Bridges[msg.Account]; ok {
				delete(srcBr.Channels, srcID)
			}
		}
	}
}

func (r *Router) mapChannelName(srcAccount, destAccount, channelName string) string {
	srcProtocol := strings.Split(srcAccount, ".")[0]
	destProtocol := strings.Split(destAccount, ".")[0]

	if srcProtocol == "discord" && destProtocol == "irc" {
		// Discord -> IRC: _ -> #
		return bDiscordToIRC(channelName)
	}
	if srcProtocol == "irc" && destProtocol == "discord" {
		// IRC DM nicks have no # prefix — sanitize for Discord
		if !strings.HasPrefix(channelName, "#") && !strings.HasPrefix(channelName, "&") {
			return sanitizeForDiscord(channelName)
		}
		// IRC -> Discord: # -> _
		return bIRCToDiscord(channelName)
	}

	return channelName
}

func bDiscordToIRC(name string) string {
	var prefixCount int
	for _, c := range name {
		if c == '_' {
			prefixCount++
		} else {
			break
		}
	}
	if prefixCount == 0 {
		return name
	}
	return strings.Repeat("#", prefixCount) + name[prefixCount:]
}

func bIRCToDiscord(name string) string {
	var prefixCount int
	for _, c := range name {
		if c == '#' {
			prefixCount++
		} else {
			break
		}
	}
	if prefixCount == 0 {
		return name
	}
	return strings.Repeat("_", prefixCount) + name[prefixCount:]
}

// syncExistingChannels emits synthetic EventChannelCreate messages for all
// existing channels on bridges that support ExistingChannelLister.
// This triggers the same dynamic bridging logic used for newly created channels,
// ensuring pre-existing channels are auto-joined at startup.
func (r *Router) syncExistingChannels() {
	for _, gw := range r.Gateways {
		for _, channel := range gw.Channels {
			if channel.Name != "*" {
				continue
			}

			account := channel.Account
			br, ok := gw.Bridges[account]
			if !ok {
				continue
			}

			lister, ok := br.Bridger.(bridge.ExistingChannelLister)
			if !ok {
				continue
			}

			r.logger.Infof("syncing existing channels from %s", account)
			for _, chName := range lister.GetExistingChannels() {
				r.logger.Debugf("emitting synthetic channel create for %s on %s", chName, account)
				r.Message <- config.Message{
					Account: account,
					Event:   config.EventChannelCreate,
					Channel: chName,
					Text:    chName,
				}
			}
		}
	}
}
