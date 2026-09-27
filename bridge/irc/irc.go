package birc

import (
	"crypto/tls"
	"errors"
	"fmt"
	"hash/crc32"
	"io/ioutil"
	"math"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/42wim/matterbridge/bridge"
	"github.com/42wim/matterbridge/bridge/config"
	"github.com/42wim/matterbridge/bridge/helper"
	"github.com/lrstanley/girc"
	stripmd "github.com/writeas/go-strip-markdown"

	// We need to import the 'data' package as an implicit dependency.
	// See: https://godoc.org/github.com/paulrosania/go-charset/charset
	_ "github.com/paulrosania/go-charset/data"
)

type Birc struct {
	i                                         *girc.Client
	Nick                                      string
	names                                     map[string][]string
	connected                                 chan error
	Local                                     chan config.Message // local queue for flood control
	FirstConnection, authDone                 bool
	MessageDelay, MessageQueue, MessageLength int
	channels                                  map[string]bool
	dmChannels                                map[string]bool
	nickServRegCache                          map[string]time.Time // account → NickServ registration date (zero = failed lookup)
	nickServLastChecked                       map[string]time.Time // account → last time the ignore decision was evaluated
	nickServIgnoreDecision                    map[string]bool      // account → cached ignore decision (true = ignore)
	nickServCacheMu                           sync.Mutex
	nickServInfoInFlight                      atomic.Int32 // number of fetchNickServRegistration queries in flight
	nickServActiveQueries                     map[string]bool   // account → a NickServ INFO query is currently in flight
	nickServRetries                           map[string]int    // account → number of consecutive NickServ INFO failures (0 = never queried / succeeded)
	pendingMessages                           map[string][]pendingMsg // messages from accounts awaiting NickServ verification
	pendingMessagesMu                         sync.Mutex
	*bridge.Config
}

// pendingMsg is a message held while NickServ registration data is being looked up.
type pendingMsg struct {
	Msg      config.Message
	QueuedAt time.Time
}

const nickServMaxHold = 15 * time.Second // max time a message is held per account before being dropped

func (b *Birc) nickServRetryDelays() []time.Duration {
	return []time.Duration{0, 2 * time.Second, 5 * time.Second}
}

func New(cfg *bridge.Config) bridge.Bridger {
	b := &Birc{}
	b.Config = cfg
	b.Nick = b.GetString("Nick")
	b.names = make(map[string][]string)
	b.connected = make(chan error)
	b.channels = make(map[string]bool)
	b.dmChannels = make(map[string]bool)
	b.nickServRegCache = make(map[string]time.Time)
	b.nickServLastChecked = make(map[string]time.Time)
	b.nickServIgnoreDecision = make(map[string]bool)
	b.nickServRetries = make(map[string]int)
	b.nickServActiveQueries = make(map[string]bool)
	b.pendingMessages = make(map[string][]pendingMsg)

	if b.GetInt("MessageDelay") == 0 {
		b.MessageDelay = 1300
	} else {
		b.MessageDelay = b.GetInt("MessageDelay")
	}
	if b.GetInt("MessageQueue") == 0 {
		b.MessageQueue = 30
	} else {
		b.MessageQueue = b.GetInt("MessageQueue")
	}
	if b.GetInt("MessageLength") == 0 {
		b.MessageLength = 400
	} else {
		b.MessageLength = b.GetInt("MessageLength")
	}
	b.FirstConnection = true
	return b
}

func (b *Birc) Command(msg *config.Message) string {
	if msg.Text == "!users" {
		b.i.Handlers.Add(girc.RPL_NAMREPLY, b.storeNames)
		b.i.Handlers.Add(girc.RPL_ENDOFNAMES, b.endNames)
		b.i.Cmd.SendRaw("NAMES " + msg.Channel) //nolint:errcheck
	}
	
	// Handle IRC commands sent from Discord using !irc prefix
	if msg.IRCCommand {
		b.Log.Infof("Executing IRC command from %s: %s", msg.Username, msg.Text)
		b.i.Cmd.SendRaw(msg.Text) //nolint:errcheck
	}
	return ""
}

func (b *Birc) Connect() error {
	if b.GetBool("UseSASL") && b.GetString("TLSClientCertificate") != "" {
		return errors.New("you can't enable SASL and TLSClientCertificate at the same time")
	}

	b.Local = make(chan config.Message, b.MessageQueue+10)
	b.Log.Infof("Connecting %s", b.GetString("Server"))

	i, err := b.getClient()
	if err != nil {
		return err
	}

	if b.GetBool("UseSASL") {
		i.Config.SASL = &girc.SASLPlain{
			User: b.GetString("NickServNick"),
			Pass: b.GetString("NickServPassword"),
		}
	}

	i.Handlers.Add(girc.RPL_WELCOME, b.handleNewConnection)
	i.Handlers.Add(girc.RPL_ENDOFMOTD, b.handleOtherAuth)
	i.Handlers.Add(girc.ERR_NOMOTD, b.handleOtherAuth)
	i.Handlers.Add(girc.ALL_EVENTS, b.handleOther)
	
	// Add handlers for numeric replies to relay to Discord
	i.Handlers.Add(girc.RPL_TOPIC, b.handleNumericReply)
	i.Handlers.Add(girc.RPL_AWAY, b.handleNumericReply)
	i.Handlers.Add(girc.RPL_WHOISUSER, b.handleNumericReply)
	i.Handlers.Add(girc.RPL_WHOISSERVER, b.handleNumericReply)
	i.Handlers.Add(girc.RPL_WHOISCHANNELS, b.handleNumericReply)
	i.Handlers.Add(girc.RPL_WHOREPLY, b.handleNumericReply)
	
	b.i = i

	go b.doConnect()

	err = <-b.connected
	if err != nil {
		return fmt.Errorf("connection failed %s", err)
	}
	b.Log.Info("Connection succeeded")
	b.FirstConnection = false
	if b.GetInt("DebugLevel") == 0 {
		i.Handlers.Clear(girc.ALL_EVENTS)
	}
	go b.doSend()
	return nil
}

func (b *Birc) Disconnect() error {
	b.i.Close()
	close(b.Local)
	return nil
}

func (b *Birc) JoinChannel(channel config.ChannelInfo) error {
	b.channels[channel.Name] = true
	// DM targets are IRC nicks (no # prefix) — no JOIN command needed
	if !strings.HasPrefix(channel.Name, "#") && !strings.HasPrefix(channel.Name, "&") {
		return nil
	}
	// need to check if we have nickserv auth done before joining channels
	for {
		if b.authDone {
			break
		}
		time.Sleep(time.Second)
	}
	if channel.Options.Key != "" {
		b.Log.Debugf("using key %s for channel %s", channel.Options.Key, channel.Name)
		b.i.Cmd.JoinKey(channel.Name, channel.Options.Key)
	} else {
		b.i.Cmd.Join(channel.Name)
	}
	return nil
}

func (b *Birc) PartChannel(channel config.ChannelInfo) error {
	delete(b.channels, channel.Name)
	// DM targets are IRC nicks — no PART command, but reset dmChannels so
	// a future DM re-triggers EventChannelCreate
	if !strings.HasPrefix(channel.Name, "#") && !strings.HasPrefix(channel.Name, "&") {
		delete(b.dmChannels, channel.Name)
		return nil
	}
	b.i.Cmd.Part(channel.Name)
	return nil
}

func (b *Birc) Send(msg config.Message) (string, error) {
	// ignore delete messages
	if msg.Event == config.EventMsgDelete {
		return "", nil
	}

	b.Log.Debugf("=> Receiving %#v", msg)

	// we can be in between reconnects #385
	if !b.i.IsConnected() {
		b.Log.Error("Not connected to server, dropping message")
		return "", nil
	}

	// Execute a command
	if strings.HasPrefix(msg.Text, "!") || msg.IRCCommand {
		b.Command(&msg)
		return "", nil
	}

	// convert to specified charset
	if err := b.handleCharset(&msg); err != nil {
		return "", err
	}

	// handle files, return if we're done here
	if ok := b.handleFiles(&msg); ok {
		return "", nil
	}

	var msgLines []string
	if b.GetBool("StripMarkdown") {
		msg.Text = stripmd.Strip(msg.Text)
	}

	if prefixFormat := b.GetString("ReplyPrefix"); len(msg.Extra["ParentMessage"]) > 0 && prefixFormat != "" {
		parentMsg := msg.Extra["ParentMessage"][0].(config.Message)
		prefix := strings.ReplaceAll(prefixFormat, "{USER}", parentMsg.Username)
		prefix = strings.ReplaceAll(prefix, "{TIME}", parentMsg.Timestamp.String())
		prefix = strings.ReplaceAll(prefix, "{MESSAGE}", parentMsg.Text[0:int(math.Min(float64(len(parentMsg.Text)), 10))])
		msg.Text = prefix + msg.Text
	}

	if b.GetBool("MessageSplit") {
		msgLines = helper.GetSubLines(msg.Text, b.MessageLength, b.GetString("MessageClipped"))
	} else {
		msgLines = helper.GetSubLines(msg.Text, 0, b.GetString("MessageClipped"))
	}
	for i := range msgLines {
		if len(b.Local) >= b.MessageQueue {
			b.Log.Debugf("flooding, dropping message (queue at %d)", len(b.Local))
			return "", nil
		}

		msg.Text = msgLines[i]
		b.Local <- msg
	}
	return "", nil
}

func (b *Birc) doConnect() {
	for {
		if err := b.i.Connect(); err != nil {
			b.Log.Errorf("disconnect: error: %s", err)
			if b.FirstConnection {
				b.connected <- err
				return
			}
		} else {
			b.Log.Info("disconnect: client requested quit")
		}
		b.Log.Info("reconnecting in 30 seconds...")
		time.Sleep(30 * time.Second)
		b.i.Handlers.Clear(girc.RPL_WELCOME)
		b.i.Handlers.Add(girc.RPL_WELCOME, func(client *girc.Client, event girc.Event) {
			b.Remote <- config.Message{Username: "system", Text: "rejoin", Channel: "", Account: b.Account, Event: config.EventRejoinChannels}
			// set our correct nick on reconnect if necessary
			b.Nick = event.Source.Name
		})
	}
}

// Sanitize nicks for RELAYMSG: replace IRC characters with special meanings with "-"
func sanitizeNick(nick string) string {
	sanitize := func(r rune) rune {
		if strings.ContainsRune("!+%@&#$:'\"?*,. ", r) {
			return '-'
		}
		return r
	}
	return strings.Map(sanitize, nick)
}

func (b *Birc) doSend() {
	rate := time.Millisecond * time.Duration(b.MessageDelay)
	throttle := time.NewTicker(rate)
	for msg := range b.Local {
		<-throttle.C
		username := msg.Username

		// Check if sending to IRC services (nickserv, alis, etc.)
		// These services expect commands without username prefix
		channel := msg.Channel
		isService := false
		serviceName := ""
		if !strings.HasPrefix(channel, "#") && !strings.HasPrefix(channel, "&") {
			// Not a channel, check if it's a known service
			serviceName = strings.ToLower(channel)
			if serviceName == "nickserv" || serviceName == "alis" || serviceName == "chanserv" || serviceName == "memoserv" || serviceName == "botserv" || serviceName == "operserv" || serviceName == "hostserv" || serviceName == "globalserv" {
				isService = true
			}
		}

		// Optional support for the proposed RELAYMSG extension, described at
		// https://github.com/jlu5/ircv3-specifications/blob/master/extensions/relaymsg.md
		// nolint:nestif
		if (b.i.HasCapability("overdrivenetworks.com/relaymsg") || b.i.HasCapability("draft/relaymsg")) &&
			b.GetBool("UseRelayMsg") && !isService {
			username = sanitizeNick(username)
			text := msg.Text

			// Work around girc chomping leading commas on single word messages?
			if strings.HasPrefix(text, ":") && !strings.ContainsRune(text, ' ') {
				text = ":" + text
			}

			if msg.Event == config.EventUserAction {
				b.i.Cmd.SendRawf("RELAYMSG %s %s :\x01ACTION %s\x01", msg.Channel, username, text) //nolint:errcheck
			} else {
				b.Log.Debugf("Sending RELAYMSG to channel %s: nick=%s", msg.Channel, username)
				b.i.Cmd.SendRawf("RELAYMSG %s %s :%s", msg.Channel, username, text) //nolint:errcheck
			}
		} else {
			if isService {
				// Send directly to service without username prefix
				b.Log.Debugf("Sending to service %s: %s", msg.Channel, msg.Text)
				b.i.Cmd.Message(msg.Channel, msg.Text)
			} else {
				if b.GetBool("Colornicks") {
					checksum := crc32.ChecksumIEEE([]byte(msg.Username))
					colorCode := checksum%14 + 2 // quick fix - prevent white or black color codes
					username = fmt.Sprintf("\x03%02d%s\x0F", colorCode, msg.Username)
				}
				switch msg.Event {
				case config.EventUserAction:
					b.i.Cmd.Action(msg.Channel, username+msg.Text)
				case config.EventNoticeIRC:
					b.Log.Debugf("Sending notice to channel %s", msg.Channel)
					b.i.Cmd.Notice(msg.Channel, username+msg.Text)
				default:
					b.Log.Debugf("Sending to channel %s", msg.Channel)
					b.i.Cmd.Message(msg.Channel, username+msg.Text)
				}
			}
		}
	}
}

// validateInput validates the server/port/nick configuration. Returns a *girc.Client if successful
func (b *Birc) getClient() (*girc.Client, error) {
	server, portstr, err := net.SplitHostPort(b.GetString("Server"))
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portstr)
	if err != nil {
		return nil, err
	}
	user := b.GetString("UserName")
	if user == "" {
		user = b.GetString("Nick")
	}
	// fix strict user handling of girc
	for !girc.IsValidUser(user) {
		if len(user) == 1 || len(user) == 0 {
			user = "matterbridge"
			break
		}
		user = user[1:]
	}
	realName := b.GetString("RealName")
	if realName == "" {
		realName = b.GetString("Nick")
	}

	debug := ioutil.Discard
	if b.GetInt("DebugLevel") == 2 {
		debug = b.Log.Writer()
	}

	pingDelay, err := time.ParseDuration(b.GetString("pingdelay"))
	if err != nil || pingDelay == 0 {
		pingDelay = time.Minute
	}

	b.Log.Debugf("setting pingdelay to %s", pingDelay)

	tlsConfig, err := b.getTLSConfig()
	if err != nil {
		return nil, err
	}

	supportedCaps := map[string][]string{"overdrivenetworks.com/relaymsg": nil, "draft/relaymsg": nil}
	if len(b.GetStringSlice("IgnoreUnregistered")) > 0 || len(b.GetStringSlice("IgnoreRegistered")) > 0 {
		supportedCaps["account-tag"] = nil
	}

	i := girc.New(girc.Config{
		Server:     server,
		ServerPass: b.GetString("Password"),
		Port:       port,
		Nick:       b.GetString("Nick"),
		User:       user,
		Name:       realName,
		SSL:        b.GetBool("UseTLS"),
		Bind:       b.GetString("Bind"),
		TLSConfig:  tlsConfig,
		PingDelay:  pingDelay,
		// skip gIRC internal rate limiting, since we have our own throttling
		AllowFlood:    true,
		Debug:         debug,
		SupportedCaps: supportedCaps,
	})
	return i, nil
}

func (b *Birc) endNames(client *girc.Client, event girc.Event) {
	channel := event.Params[1]
	sort.Strings(b.names[channel])
	maxNamesPerPost := (300 / b.nicksPerRow()) * b.nicksPerRow()
	for len(b.names[channel]) > maxNamesPerPost {
		b.Remote <- config.Message{
			Username: b.Nick, Text: b.formatnicks(b.names[channel][0:maxNamesPerPost]),
			Channel: channel, Account: b.Account,
		}
		b.names[channel] = b.names[channel][maxNamesPerPost:]
	}
	b.Remote <- config.Message{
		Username: b.Nick, Text: b.formatnicks(b.names[channel]),
		Channel: channel, Account: b.Account,
	}
	b.names[channel] = nil
	b.i.Handlers.Clear(girc.RPL_NAMREPLY)
	b.i.Handlers.Clear(girc.RPL_ENDOFNAMES)
}

func (b *Birc) skipPrivMsg(event girc.Event) bool {
	// Our nick can be changed
	b.Nick = b.i.GetNick()

	// freenode doesn't send 001 as first reply
	if event.Command == "NOTICE" && len(event.Params) != 2 {
		return true
	}
	// don't forward queries to the bot (unless DirectMessages is enabled)
	if event.Params[0] == b.Nick {
		if !b.GetBool("DirectMessages") {
			return true
		}
		// DirectMessages = true: fall through and process the DM
	}
	// don't forward message from ourself
	if event.Source != nil {
		if event.Source.Name == b.Nick {
			return true
		}
	}

	// don't forward messages we sent via RELAYMSG
	if relayedNick, ok := event.Tags.Get("draft/relaymsg"); ok && relayedNick == b.Nick {
		return true
	}
	// This is the old name of the cap sent in spoofed messages; I've kept this in
	// for compatibility reasons
	if relayedNick, ok := event.Tags.Get("relaymsg"); ok && relayedNick == b.Nick {
		return true
	}

	// check if we have any parameters
	if len(event.Params) == 0 {
		return true
	}

	whitelist := b.GetStringSlice("IgnoreWhitelist")
	for _, nick := range whitelist {
		if strings.EqualFold(nick, event.Source.Name) {
			return false
		}
	}

	return b.skipByAccountTag(event, b.i.HasCapability("account-tag"))
}

// accountFromTag returns the services account name carried by an IRCv3
// account tag, and whether the sender is actually registered with services.
// Per the IRCv3 account-tag spec, users who are NOT logged in to services
// still carry the tag, with the value "*", so the presence of the tag alone
// does not prove registration. The second return value is false when the
// tag is absent, empty, or "*".
func accountFromTag(tags girc.Tags) (account string, registered bool) {
	account, ok := tags.Get("account")
	if !ok || account == "" || account == "*" {
		return "", false
	}
	return account, true
}

// skipByAccountTag applies the account-tag based filters (IgnoreUnregistered
// and IgnoreRegistered) to a PRIVMSG. hasAccountTag reports whether the
// account-tag capability is enabled; it is passed in so the decision can be
// unit-tested without a girc client (girc.Client.HasCapability reports false
// while disconnected). It returns true when the message must be skipped.
func (b *Birc) skipByAccountTag(event girc.Event, hasAccountTag bool) bool {
	// IgnoreUnregistered: ignore messages from users not logged in to services
	for _, c := range b.GetStringSlice("IgnoreUnregistered") {
		if c != "*" && !strings.EqualFold(c, event.Params[0]) {
			continue
		}
		if hasAccountTag {
			if _, registered := accountFromTag(event.Tags); !registered {
				b.Log.Debugf("Ignoring message from %s in %s (unregistered).", event.Source.Name, event.Params[0])
				return true
			}
		}
		break
	}

	// IgnoreRegistered: ignore messages from recently registered accounts.
	// Recency only applies to named accounts; an unregistered sender (no
	// tag, "*" or empty) passes through here and is left to IgnoreUnregistered.
	ignoreRegisteredChannels := b.GetStringSlice("IgnoreRegistered")
	ignoreDays := b.GetInt("IgnoreRegisteredDays")
	if len(ignoreRegisteredChannels) == 0 || ignoreDays <= 0 || !hasAccountTag {
		return false
	}
	for _, c := range ignoreRegisteredChannels {
		if c != "*" && !strings.EqualFold(c, event.Params[0]) {
			continue
		}
		if account, registered := accountFromTag(event.Tags); registered {
			return b.skipRecentlyRegistered(account, event, ignoreDays)
		}
		break
	}

	return false
}

// recentlyRegisteredDecision holds the outcome of evaluating an account against
// the IgnoreRegistered filter.
type recentlyRegisteredDecision struct {
	shouldIgnore bool // account is registered less than ignoreDays ago
	hold         bool // message must be held pending NickServ verification
	gaveUp       bool // verification failed too many times; messages pass through
	fire         bool // whether to start the next verification attempt
	retry        int  // index into nickServRetryDelays when fire is true
}

// evaluateRecentlyRegistered computes the IgnoreRegistered decision for a message
// from the given account and updates the NickServ tracking state accordingly.
// It does not queue messages, fire queries, or send anything, so it can be
// unit-tested without a girc client.
func (b *Birc) evaluateRecentlyRegistered(account string, ignoreDays int) recentlyRegisteredDecision {
	b.nickServCacheMu.Lock()
	defer b.nickServCacheMu.Unlock()

	regDate, cached := b.nickServRegCache[account]
	checkedRecently := time.Since(b.nickServLastChecked[account]) < 24*time.Hour
	activeQuery := b.nickServActiveQueries[account]
	retries := b.nickServRetries[account]
	maxRetries := len(b.nickServRetryDelays())

	var d recentlyRegisteredDecision

	switch {
	case cached && checkedRecently:
		// Reuse the cached decision from today
		d.shouldIgnore = b.nickServIgnoreDecision[account]
	case cached:
		// Registration date known but the 24h decision window expired —
		// re-evaluate synchronously from the known date
		d.shouldIgnore = time.Since(regDate).Hours()/24 < float64(ignoreDays)
		b.nickServLastChecked[account] = time.Now()
		b.nickServIgnoreDecision[account] = d.shouldIgnore
	case activeQuery:
		// A verification query is in flight — hold until it completes and
		// flushes the pending messages (don't start another query)
		d.hold = true
	case retries >= maxRetries:
		if checkedRecently {
			// Gave up on verification after too many failures — allow through
			// until the 24h window expires, then verify again
			d.gaveUp = true
		} else {
			b.nickServRetries[account] = 1
			b.nickServLastChecked[account] = time.Now()
			d.hold = true
			d.fire = true
			d.retry = 0
		}
	default:
		// Unknown account (first sight or retrying after a failure) —
		// hold and fire the next verification attempt
		d.retry = retries
		b.nickServRetries[account] = retries + 1
		if !checkedRecently {
			b.nickServLastChecked[account] = time.Now()
		}
		d.hold = true
		d.fire = true
	}

	return d
}

// skipRecentlyRegistered decides whether a message from a registered account should be
// ignored (account registered less than ignoreDays ago), held pending NickServ
// verification, or allowed through.
func (b *Birc) skipRecentlyRegistered(account string, event girc.Event, ignoreDays int) bool {
	d := b.evaluateRecentlyRegistered(account, ignoreDays)

	switch {
	case d.hold:
		// Queue first so the message is present if the query completes very fast
		b.queueMessageForAccount(account, event)
		if d.fire {
			// Mark the query active synchronously so a burst of messages from
			// the same account doesn't each fire its own NICKSERV INFO (the flag
			// used to be set inside the query goroutine, after a window where
			// concurrent messages all saw activeQuery=false).
			b.nickServCacheMu.Lock()
			b.nickServActiveQueries[account] = true
			b.nickServCacheMu.Unlock()
			delay := b.nickServRetryDelays()[d.retry]
			b.Log.Debugf("Holding message from %s (account %s) pending NickServ verification (attempt %d, delay %v)", event.Source.Name, account, d.retry, delay)
			go func() {
				if delay > 0 {
					time.Sleep(delay)
				}
				b.fetchNickServRegistration(account)
			}()
		} else {
			b.Log.Debugf("Holding message from %s (account %s) pending NickServ verification", event.Source.Name, account)
		}
		return true
	case d.gaveUp:
		b.Log.Warnf("Giving up NickServ verification for account %s, allowing messages through for now", account)
		return false
	case d.shouldIgnore:
		b.Log.Debugf("Ignoring message from %s (account %s, registered less than %d days ago)", event.Source.Name, account, ignoreDays)
		return true
	default:
		return false
	}
}

// queueMessageForAccount stores a message from an account that is pending NickServ verification
func (b *Birc) queueMessageForAccount(account string, event girc.Event) {
	b.pendingMessagesMu.Lock()
	defer b.pendingMessagesMu.Unlock()

	channel := event.Params[0]
	if channel == b.Nick && b.GetBool("DirectMessages") {
		channel = strings.ToLower(event.Source.Name)
	}

	msg := config.Message{
		Username: event.Source.Name,
		Channel:  channel,
		Account:  b.Account,
		UserID:   event.Source.Ident + "@" + event.Source.Host,
		Text:     event.StripAction(),
	}

	// Set action event if this is an ACTION
	if event.IsAction() {
		msg.Event = config.EventUserAction
	}

	// Set NOTICE event
	if event.Command == "NOTICE" {
		msg.Event = config.EventNoticeIRC
	}

	b.pendingMessages[account] = append(b.pendingMessages[account], pendingMsg{
		Msg:      msg,
		QueuedAt: time.Now(),
	})
	b.Log.Debugf("Queued message from %s (account %s) pending NickServ verification", event.Source.Name, account)
}

// processPendingMessages flushes all queued messages for an account after NickServ
// verification completes. Messages held longer than nickServMaxHold are dropped.
func (b *Birc) processPendingMessages(account string) {
	b.pendingMessagesMu.Lock()
	messages := b.pendingMessages[account]
	delete(b.pendingMessages, account)
	b.pendingMessagesMu.Unlock()

	if len(messages) == 0 {
		return
	}

	ignoreDays := b.GetInt("IgnoreRegisteredDays")
	ignoreRegisteredChannels := b.GetStringSlice("IgnoreRegistered")

	// Check if this account should be ignored based on registration age
	shouldIgnore := false
	b.nickServCacheMu.Lock()
	if regDate, ok := b.nickServRegCache[account]; ok && !regDate.IsZero() {
		ageDays := time.Since(regDate).Hours() / 24
		shouldIgnore = ageDays < float64(ignoreDays)
	}
	b.nickServCacheMu.Unlock()

	b.Log.Debugf("Flushing %d pending messages for account %s (shouldIgnore=%v)", len(messages), account, shouldIgnore)

	for _, pm := range messages {
		msg := pm.Msg

		// Bounded hold: drop messages that waited too long for verification
		if time.Since(pm.QueuedAt) > nickServMaxHold {
			b.Log.Debugf("Dropping held message from %s (account %s), waited longer than %s", msg.Username, account, nickServMaxHold)
			continue
		}

		// Check if the message channel is in the ignore list
		channelMatches := false
		for _, c := range ignoreRegisteredChannels {
			if c == "*" || strings.EqualFold(c, msg.Channel) {
				channelMatches = true
				break
			}
		}

		// Only send the message if it's not from an ignored account/channel combination
		if !(channelMatches && shouldIgnore) {
			b.Log.Debugf("Forwarding held message from %s (account %s)", msg.Username, account)
			b.Remote <- msg
		} else {
			b.Log.Debugf("Ignoring held message from %s (account %s, too recently registered)", msg.Username, account)
		}
	}
}

func (b *Birc) nicksPerRow() int {
	return 4
}

func (b *Birc) storeNames(client *girc.Client, event girc.Event) {
	channel := event.Params[2]
	b.names[channel] = append(
		b.names[channel],
		strings.Split(strings.TrimSpace(event.Last()), " ")...)
}

func (b *Birc) formatnicks(nicks []string) string {
	return strings.Join(nicks, ", ") + " currently on IRC"
}

// nickServName returns the configured NickServ bot name, defaulting to "NickServ".
func (b *Birc) nickServName() string {
	if n := b.GetString("NickServNick"); n != "" {
		return n
	}
	return "NickServ"
}

// fetchNickServRegistration queries NickServ for the registration date of the given account,
// caches the result (or a zero time on failure), and never re-queries the same account.
// It is intended to be called in a goroutine.
func (b *Birc) fetchNickServRegistration(account string) {
	nsName := b.nickServName()
	b.Log.Debugf("Querying %s for registration date of account %s", nsName, account)

	// Mark this account as having an active query
	b.nickServCacheMu.Lock()
	b.nickServActiveQueries[account] = true
	b.nickServCacheMu.Unlock()

	b.nickServInfoInFlight.Add(1)
	defer b.nickServInfoInFlight.Add(-1)

	// Clean up the active query when done
	defer func() {
		b.nickServCacheMu.Lock()
		delete(b.nickServActiveQueries, account)
		b.nickServCacheMu.Unlock()
	}()

	b.i.Cmd.Message(nsName, "INFO "+account) //nolint:errcheck

	regDate, found := b.collectNickServLines(nsName)
	b.finishNickServRegistration(account, regDate, found)
}

// nickservLine is the parsed classification of a single NICKSERV INFO NOTICE line.
type nickservLine struct {
	regDate time.Time
	found   bool
	term    bool // terminating line (End of Info / not registered)
}

// classifyNickServLine parses one raw NICKSERV NOTICE line into its
// classification. It is pure so it can be unit-tested without a girc client.
func classifyNickServLine(text string) nickservLine {
	var line nickservLine
	// Parse the registration date line (contains "registered" case-insensitively)
	if strings.Contains(strings.ToLower(text), "registered") {
		if t, err := parseNickServDate(text); err == nil {
			line.regDate = t
			line.found = true
		}
	}
	// End-of-INFO markers from Atheme and Anope (strip formatting before comparing)
	if strings.Contains(text, "*** End of Info ***") ||
		strings.Contains(text, "isn't registered") ||
		strings.Contains(text, "is not registered") {
		line.term = true
	}
	return line
}

// accumulateNickServLines folds a sequence of classified lines into a single
// result; the first line that carries a registration date wins. Pure.
func accumulateNickServLines(lines []nickservLine) (regDate time.Time, found bool) {
	for _, l := range lines {
		if l.found && !found {
			found = true
			regDate = l.regDate
		}
	}
	return regDate, found
}

// collectNickServLines registers a temporary NOTICE handler and accumulates the
// NICKSERV INFO reply lines until a terminating line arrives or the deadline
// elapses. This is the thin girc transport; the line parsing and accumulation
// are done by the pure classifyNickServLine/accumulateNickServLines so the
// meaningful logic is testable independently of girc's async handler scheduling.
func (b *Birc) collectNickServLines(nsName string) (regDate time.Time, found bool) {
	lines := make(chan nickservLine, 16)

	b.i.Handlers.AddTmp(girc.NOTICE, 2*time.Second, func(c *girc.Client, e girc.Event) bool {
		if e.Source == nil || !strings.EqualFold(e.Source.Name, nsName) {
			return false
		}
		line := classifyNickServLine(girc.StripRaw(e.Last()))
		// Non-blocking send: never let the handler goroutine block, even if we've
		// already stopped reading (e.g. after the timeout below).
		select {
		case lines <- line:
		default:
		}
		return line.term
	})

	acc := make([]nickservLine, 0, 16)
	deadline := time.After(2 * time.Second)
	for {
		select {
		case line := <-lines:
			acc = append(acc, line)
			if line.term {
				return accumulateNickServLines(acc)
			}
		case <-deadline:
			return accumulateNickServLines(acc)
		}
	}
}

// finishNickServRegistration applies the outcome of a NickServ lookup to the
// cache/decision state and flushes any pending messages for the account.
func (b *Birc) finishNickServRegistration(account string, regDate time.Time, found bool) {
	b.nickServCacheMu.Lock()
	if found {
		b.nickServRegCache[account] = regDate
		b.nickServLastChecked[account] = time.Now()
		delete(b.nickServRetries, account) // Clear retry counter on success
		// Set the ignore decision based on account age
		ignoreDays := b.GetInt("IgnoreRegisteredDays")
		ageDays := time.Since(regDate).Hours() / 24
		b.nickServIgnoreDecision[account] = ageDays < float64(ignoreDays)
		b.Log.Debugf("NickServ: account %s registered on %s (ignore decision: %v)", account, regDate.Format(time.RFC3339), b.nickServIgnoreDecision[account])
	} else {
		b.Log.Warnf("Could not fetch NickServ registration date for account %s (query failed or timed out, attempt %d)", account, b.nickServRetries[account])
	}
	b.nickServCacheMu.Unlock()

	// Process any pending messages for this account
	b.processPendingMessages(account)
}

// parseNickServDate extracts and parses the registration date from a NickServ INFO NOTICE line.
// It handles common formats from Atheme (Libera.chat) and Anope.
func parseNickServDate(line string) (time.Time, error) {
	// Find the colon separating the field name from the value and take everything after it.
	// Examples:
	//   "Registered : Jan 02 00:00:00 2020 UTC"   (Atheme)
	//   "Registered: Mon Jan 02 00:00:00 2020"     (Anope)
	//   "Time registered : Jan 02 00:00:00 2020 UTC"
	idx := strings.Index(line, ":")
	if idx < 0 {
		return time.Time{}, fmt.Errorf("no colon in NickServ line: %q", line)
	}
	dateStr := strings.TrimSpace(line[idx+1:])

	// Libera/Atheme appends a human-readable age suffix in parentheses, e.g.:
	//   "Jul 03 18:30:15 2021 +0000 (4y 36w 3d ago)"
	// Strip everything from the first " (" onward.
	if i := strings.Index(dateStr, " ("); i >= 0 {
		dateStr = strings.TrimSpace(dateStr[:i])
	}

	formats := []string{
		"Jan 02 15:04:05 2006 MST",   // Atheme with named timezone (e.g. UTC)
		"Jan 02 15:04:05 2006 -0700", // Atheme with numeric offset
		"Jan 02 15:04:05 2006",       // Atheme without timezone
		"Mon Jan 02 15:04:05 2006",   // Anope
		"Jan 02 2006 15:04:05",       // alternate
	}
	for _, f := range formats {
		if t, err := time.Parse(f, dateStr); err == nil {
			return t, nil
		}
	}
	// Some networks store a Unix timestamp
	if ts, err := strconv.ParseInt(dateStr, 10, 64); err == nil {
		return time.Unix(ts, 0), nil
	}
	return time.Time{}, fmt.Errorf("unrecognized NickServ date format: %q", dateStr)
}

func (b *Birc) getTLSConfig() (*tls.Config, error) {
	server, _, _ := net.SplitHostPort(b.GetString("server"))

	tlsConfig := &tls.Config{
		InsecureSkipVerify: b.GetBool("skiptlsverify"), //nolint:gosec
		ServerName:         server,
	}

	if filename := b.GetString("TLSClientCertificate"); filename != "" {
		cert, err := tls.LoadX509KeyPair(filename, filename)
		if err != nil {
			return nil, err
		}

		tlsConfig.Certificates = []tls.Certificate{cert}
	}

	return tlsConfig, nil
}
