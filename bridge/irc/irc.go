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
	nickServQueried                           map[string]bool      // accounts already queried; prevents re-querying on failure
	nickServLastChecked                       map[string]time.Time // account → last time the ignore decision was evaluated
	nickServIgnoreDecision                    map[string]bool      // account → cached ignore decision (true = ignore)
	nickServCacheMu                           sync.Mutex
	nickServInfoInFlight                      atomic.Int32                // number of fetchNickServRegistration queries in flight
	nickServActiveQueries                     map[string]bool             // track accounts with active NickServ queries
	nickServRetries                           map[string]int              // account → number of NickServ INFO retry attempts
	pendingMessages                           map[string][]config.Message // messages from accounts awaiting NickServ verification
	pendingMessagesMu                         sync.Mutex
	*bridge.Config
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
	b.nickServQueried = make(map[string]bool)
	b.nickServLastChecked = make(map[string]time.Time)
	b.nickServIgnoreDecision = make(map[string]bool)
	b.nickServRetries = make(map[string]int)
	b.nickServActiveQueries = make(map[string]bool)
	b.pendingMessages = make(map[string][]config.Message)

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
	if strings.HasPrefix(msg.Text, "!") {
		b.Command(&msg)
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

	ignoreChannels := b.GetStringSlice("IgnoreUnregistered")
	shouldIgnore := false
	for _, c := range ignoreChannels {
		if c == "*" || strings.EqualFold(c, event.Params[0]) {
			shouldIgnore = true
			break
		}
	}

	if shouldIgnore {
		if b.i.HasCapability("account-tag") {
			_, ok := event.Tags.Get("account")
			if !ok {
				b.Log.Debugf("Ignoring message from %s in %s (unregistered).", event.Source.Name, event.Params[0])
				return true
			}
		}
	}

	// IgnoreRegistered: ignore messages from recently registered accounts
	ignoreRegisteredChannels := b.GetStringSlice("IgnoreRegistered")
	ignoreDays := b.GetInt("IgnoreRegisteredDays")
	if len(ignoreRegisteredChannels) > 0 && ignoreDays > 0 && b.i.HasCapability("account-tag") {
		channelMatches := false
		for _, c := range ignoreRegisteredChannels {
			if c == "*" || strings.EqualFold(c, event.Params[0]) {
				channelMatches = true
				break
			}
		}
		if channelMatches {
			if account, ok := event.Tags.Get("account"); ok {
				b.nickServCacheMu.Lock()
				regDate, cached := b.nickServRegCache[account]
				lastChecked := b.nickServLastChecked[account]
				checkedRecently := time.Since(lastChecked) < 24*time.Hour
				cachedDecision := b.nickServIgnoreDecision[account]

				var shouldIgnore bool
				if cached {
					if checkedRecently {
						// Reuse the cached decision from today
						shouldIgnore = cachedDecision
					} else {
						// Re-evaluate and store the new decision
						if !regDate.IsZero() {
							ageDays := time.Since(regDate).Hours() / 24
							shouldIgnore = ageDays < float64(ignoreDays)
						}
						b.nickServLastChecked[account] = time.Now()
						b.nickServIgnoreDecision[account] = shouldIgnore
					}
				} else if !checkedRecently && !b.nickServActiveQueries[account] {
					// First time seeing this account or not checked in 24 hours
					// Set lastChecked timestamp (retry count will be read and incremented before fetch)
					b.nickServLastChecked[account] = time.Now()
					b.nickServQueried[account] = true
				}
				b.nickServCacheMu.Unlock()

				if cached {
					if shouldIgnore {
						b.Log.Debugf("Ignoring message from %s (account %s, cached ignore decision)",
							event.Source.Name, account)
						return true
					}
					// zero regDate = failed lookup, or account is old enough — allow through
				} else if !checkedRecently && !b.nickServActiveQueries[account] {
					// First time seeing this account or not checked in 24 hours — fire background NickServ query, HOLD message
					retryCount := b.nickServRetries[account]
					var delay time.Duration
					switch retryCount {
					case 0:
						delay = 0
					case 1:
						delay = 2 * time.Second
					case 2:
						delay = 5 * time.Second
					default:
						// Give up after 3 retries, let message through
						b.Log.Warnf("Giving up NickServ verification for account %s after %d retries, allowing messages through", account, retryCount)
					}
					if retryCount < 3 {
						b.nickServRetries[account]++
						b.Log.Debugf("Holding message from %s (account %s) pending NickServ verification (retry %d, delay %v)", event.Source.Name, account, retryCount, delay)
						b.queueMessageForAccount(account, event)
						go func() {
							if delay > 0 {
								time.Sleep(delay)
							}
							b.fetchNickServRegistration(account)
						}()
						return true
					}
				}
			}
		}
	}

	return false
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

	b.pendingMessages[account] = append(b.pendingMessages[account], msg)
	b.Log.Debugf("Queued message from %s (account %s) pending NickServ verification", event.Source.Name, account)
}

// processPendingMessages processes all queued messages for an account after NickServ verification
func (b *Birc) processPendingMessages(account string) {
	b.pendingMessagesMu.Lock()
	defer b.pendingMessagesMu.Unlock()

	messages, exists := b.pendingMessages[account]
	if !exists {
		return
	}

	ignoreDays := b.GetInt("IgnoreRegisteredDays")
	ignoreRegisteredChannels := b.GetStringSlice("IgnoreRegistered")

	// Check if this account should be ignored based on registration age
	shouldIgnore := false
	if regDate, ok := b.nickServRegCache[account]; ok && !regDate.IsZero() {
		ageDays := time.Since(regDate).Hours() / 24
		shouldIgnore = ageDays < float64(ignoreDays)
	}

	b.Log.Debugf("Processing %d pending messages for account %s (shouldIgnore=%v)", len(messages), account, shouldIgnore)

	for _, msg := range messages {
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
	// Clear the pending messages for this account
	delete(b.pendingMessages, account)
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

	var regDate time.Time
	found := false

	_, done := b.i.Handlers.AddTmp(girc.NOTICE, 2*time.Second, func(c *girc.Client, e girc.Event) bool {
		src := ""
		if e.Source != nil {
			src = e.Source.Name
		}
		b.Log.Debugf("fetchNickServRegistration AddTmp fired: source=%q params=%v text=%q (want source=%q)", src, e.Params, e.Last(), nsName)
		if e.Source == nil || !strings.EqualFold(e.Source.Name, nsName) {
			return false
		}
		text := girc.StripRaw(e.Last())
		// Parse the registration date line (contains "registered" case-insensitively)
		if !found && strings.Contains(strings.ToLower(text), "registered") {
			if t, err := parseNickServDate(text); err == nil {
				regDate = t
				found = true
			} else {
				b.Log.Debugf("fetchNickServRegistration: failed to parse date from %q: %v", text, err)
			}
		}
		// End-of-INFO markers from Atheme and Anope (strip formatting before comparing)
		if strings.Contains(text, "*** End of Info ***") ||
			strings.Contains(text, "isn't registered") ||
			strings.Contains(text, "is not registered") {
			return true // remove handler
		}
		return false
	})

	<-done

	b.nickServCacheMu.Lock()
	if found {
		b.nickServRegCache[account] = regDate
		b.nickServLastChecked[account] = time.Now()
		b.nickServRetries[account] = 0 // Clear retry counter on success
		// Set the ignore decision based on account age
		ignoreDays := b.GetInt("IgnoreRegisteredDays")
		ageDays := time.Since(regDate).Hours() / 24
		b.nickServIgnoreDecision[account] = ageDays < float64(ignoreDays)
		b.Log.Debugf("NickServ: account %s registered on %s (ignore decision: %v)", account, regDate.Format(time.RFC3339), b.nickServIgnoreDecision[account])
	} else {
		b.Log.Warnf("Could not fetch NickServ registration date for account %s (query failed or timed out, retry %d)", account, b.nickServRetries[account])
	}

	// Process any pending messages for this account
	b.processPendingMessages(account)
	b.nickServCacheMu.Unlock()
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
