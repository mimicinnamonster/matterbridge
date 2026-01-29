package gateway

import (
	"strings"

	"github.com/42wim/matterbridge/bridge/config"
)

func (r *Router) handleChannelCreate(msg *config.Message) {
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
	r.logger.Debugf("handleChannelDelete: %#v", msg)
	// TODO: implement dynamic channel removal if needed
	// For now, we mainly care about creation.
}

func (r *Router) mapChannelName(srcAccount, destAccount, channelName string) string {
	srcProtocol := strings.Split(srcAccount, ".")[0]
	destProtocol := strings.Split(destAccount, ".")[0]

	if srcProtocol == "discord" && destProtocol == "irc" {
		// Discord -> IRC: _ -> #
		return bDiscordToIRC(channelName)
	}
	if srcProtocol == "irc" && destProtocol == "discord" {
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
	return strings.Repeat("_", prefixCount-1) + name[prefixCount:]
}
