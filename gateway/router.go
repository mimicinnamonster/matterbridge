package gateway

import (
	"fmt"
	"sync"
	"time"

	"github.com/42wim/matterbridge/bridge"
	"github.com/42wim/matterbridge/bridge/config"
	"github.com/42wim/matterbridge/gateway/samechannel"
	"github.com/hashicorp/golang-lru"
	"github.com/sirupsen/logrus"
)

type Router struct {
	config.Config
	sync.RWMutex

	BridgeMap        map[string]bridge.Factory
	Gateways         map[string]*Gateway
	Message          chan config.Message
	MattermostPlugin chan config.Message

	// dedupeCache tracks recently seen (account, channel, username, text)
	// combinations for protocols without native message IDs (e.g. IRC).
	// ZNC replays buffered messages after a reconnect, which would otherwise
	// relay every message twice to all destinations.
	dedupeCache *lru.Cache

	logger *logrus.Entry
}

// NewRouter initializes a new Matterbridge router for the specified configuration and
// sets up all required gateways.
func NewRouter(rootLogger *logrus.Logger, cfg config.Config, bridgeMap map[string]bridge.Factory) (*Router, error) {
	logger := rootLogger.WithFields(logrus.Fields{"prefix": "router"})

	dedupeCache, _ := lru.New(10000)
	r := &Router{
		Config:           cfg,
		BridgeMap:        bridgeMap,
		Message:          make(chan config.Message),
		MattermostPlugin: make(chan config.Message),
		Gateways:         make(map[string]*Gateway),
		dedupeCache:      dedupeCache,
		logger:           logger,
	}
	sgw := samechannel.New(cfg)
	gwconfigs := append(sgw.GetConfig(), cfg.BridgeValues().Gateway...)

	for idx := range gwconfigs {
		entry := &gwconfigs[idx]
		if !entry.Enable {
			continue
		}
		if entry.Name == "" {
			return nil, fmt.Errorf("%s", "Gateway without name found")
		}
		if _, ok := r.Gateways[entry.Name]; ok {
			return nil, fmt.Errorf("Gateway with name %s already exists", entry.Name)
		}
		r.Gateways[entry.Name] = New(rootLogger, entry, r)
	}
	return r, nil
}

// Start will connect all gateways belonging to this router and subsequently route messages
// between them.
func (r *Router) Start() error {
	m := make(map[string]*bridge.Bridge)
	if len(r.Gateways) == 0 {
		return fmt.Errorf("no [[gateway]] configured. See https://github.com/42wim/matterbridge/wiki/How-to-create-your-config for more info")
	}
	for _, gw := range r.Gateways {
		r.logger.Infof("Parsing gateway %s", gw.Name)
		if len(gw.Bridges) == 0 {
			return fmt.Errorf("no bridges configured for gateway %s. See https://github.com/42wim/matterbridge/wiki/How-to-create-your-config for more info", gw.Name)
		}
		for _, br := range gw.Bridges {
			m[br.Account] = br
		}
	}
	for _, br := range m {
		r.logger.Infof("Starting bridge: %s ", br.Account)
		err := br.Connect()
		if err != nil {
			e := fmt.Errorf("Bridge %s failed to start: %v", br.Account, err)
			if r.disableBridge(br, e) {
				continue
			}
			return e
		}
		err = br.JoinChannels()
		if err != nil {
			e := fmt.Errorf("Bridge %s failed to join channel: %v", br.Account, err)
			if r.disableBridge(br, e) {
				continue
			}
			return e
		}
	}
	// remove unused bridges
	for _, gw := range r.Gateways {
		for i, br := range gw.Bridges {
			if br.Bridger == nil {
				r.logger.Errorf("removing failed bridge %s", i)
				delete(gw.Bridges, i)
			}
		}
	}
	go r.handleReceive()
	go r.syncExistingChannels()
	//go r.updateChannelMembers()
	return nil
}

// disableBridge returns true and empties a bridge if we have IgnoreFailureOnStart configured
// otherwise returns false
func (r *Router) disableBridge(br *bridge.Bridge, err error) bool {
	if r.BridgeValues().General.IgnoreFailureOnStart {
		r.logger.Error(err)
		// setting this bridge empty
		*br = bridge.Bridge{
			Log: br.Log,
		}
		return true
	}
	return false
}

func (r *Router) getBridge(account string) *bridge.Bridge {
	for _, gw := range r.Gateways {
		if br, ok := gw.Bridges[account]; ok {
			return br
		}
	}
	return nil
}

func (r *Router) handleReceive() {
	for msg := range r.Message {
		msg := msg // scopelint
		r.handleEventGetChannelMembers(&msg)
		r.handleEventFailure(&msg)
		r.handleEventRejoinChannels(&msg)
		r.handleChannelCreate(&msg)
		r.handleChannelDelete(&msg)

		if msg.Event == config.EventChannelCreate || msg.Event == config.EventChannelDelete {
			continue
		}

		// Drop duplicate messages from ID-less protocols (e.g. IRC/ZNC buffer
		// replay after a reconnect). Messages with a native ID (Discord, ...)
		// are never deduplicated here.
		if msg.Text != "" && msg.ID == "" &&
			(msg.Event == "" || msg.Event == config.EventUserAction) &&
			r.isDuplicateMessage(&msg) {
			r.logger.Debugf("dropping duplicate message %q from %s on %s (channel %s)",
				msg.Text, msg.Username, msg.Account, msg.Channel)
			continue
		}

		// Set message protocol based on the account it came from
		msg.Protocol = r.getBridge(msg.Account).Protocol

		filesHandled := false
		for _, gw := range r.Gateways {
			// record all the message ID's of the different bridges
			var msgIDs []*BrMsgID
			if gw.ignoreMessage(&msg) {
				continue
			}
			msg.Timestamp = time.Now()
			gw.modifyMessage(&msg)
			if !filesHandled {
				gw.handleFiles(&msg)
				filesHandled = true
			}
			for _, br := range gw.Bridges {
				msgIDs = append(msgIDs, gw.handleMessage(&msg, br)...)
			}

			if msg.ID != "" {
				_, exists := gw.Messages.Get(msg.Protocol + " " + msg.ID)

				// Only add the message ID if it doesn't already exist
				//
				// For some bridges we always add/update the message ID.
				// This is necessary as msgIDs will change if a bridge returns
				// a different ID in response to edits.
				if !exists {
					gw.Messages.Add(msg.Protocol+" "+msg.ID, msgIDs)
				}
			}
		}
	}
}

// isDuplicateMessage reports whether an identical message (same account,
// channel, username and text) was seen within the dedupe window. It is
// used to filter out messages that are delivered twice by bridges without
// native message IDs, e.g. IRC messages replayed by a ZNC buffer after a
// reconnect. The window defaults to 5 seconds and can be tuned with
// [general] DedupeSeconds (0 disables deduplication).
func (r *Router) isDuplicateMessage(msg *config.Message) bool {
	window := time.Duration(r.BridgeValues().General.DedupeSeconds) * time.Second
	if window == 0 {
		return false
	}

	key := msg.Account + "\x00" + msg.Channel + "\x00" + msg.Username + "\x00" + msg.Text
	now := time.Now()
	if last, ok := r.dedupeCache.Get(key); ok {
		if now.Sub(last.(time.Time)) < window {
			// Don't refresh the timestamp on a hit, so a legitimate repeat of
			// the same text after the window has passed isn't dropped.
			return true
		}
	}
	r.dedupeCache.Add(key, now)
	return false
}

// updateChannelMembers sends every minute an GetChannelMembers event to all bridges.
func (r *Router) updateChannelMembers() {
	// TODO sleep a minute because slack can take a while
	// fix this by having actually connectionDone events send to the router
	time.Sleep(time.Minute)
	for {
		for _, gw := range r.Gateways {
			for _, br := range gw.Bridges {
				// only for slack now
				if br.Protocol != "slack" {
					continue
				}
				r.logger.Debugf("sending %s to %s", config.EventGetChannelMembers, br.Account)
				if _, err := br.Send(config.Message{Event: config.EventGetChannelMembers}); err != nil {
					r.logger.Errorf("updateChannelMembers: %s", err)
				}
			}
		}
		time.Sleep(time.Minute)
	}
}
