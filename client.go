package xmpp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"reflect"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	xmpp "github.com/mattn/go-xmpp"
)

type addr struct {
	host string
	port int
}

var addrs = [...]addr{
	addr{host: "fcm-xmpp.googleapis.com", port: 5235}, // Prod
	addr{host: "fcm-xmpp.googleapis.com", port: 5236}, // PreProd
}

// Endpoint is the endpoint type. Options: Prod and PreProd
type Endpoint int

const (
	// Prod is production endpoint.
	Prod Endpoint = iota

	// PreProd is pre-production endpoint.
	PreProd
)

// Addr return the host and port.
func (e Endpoint) Addr() (string, int) {
	v := addrs[e]
	return v.host, v.port
}

const stanzaFmt = `<message id="%s"><gcm xmlns="google:mobile:data">%v</gcm></message>`
const duration4Weeks = 4 * 7 * 24 * time.Hour

const (
	// StateConnected which the connection establish and ready to be use.
	StateConnected int32 = iota

	// StateDraining which the connection is draining, should no any new message sent from here.
	StateDraining

	// StateClosing which the connection is in closing state.
	StateClosing

	// StateClosed which the connection already closed.
	StateClosed
)

const codeDraining = "CONNECTION_DRAINING"

// DefaultXMPPClientFactory is the default XMPPClientFactory.
var DefaultXMPPClientFactory = RealXMPPClientFactory{}

// Client of the GCM.
type Client struct { // nolint: maligned
	host  string
	debug bool

	client     XMPPClient
	wg         sync.WaitGroup
	outMessage chan struct{}
	state      int32
	done       chan struct{}

	pendingMessagesMu sync.RWMutex
	pendingMessages   map[string]struct{}

	pingMu sync.Mutex
	pong   chan struct{}
}

// NewClient constructs new Client.
func NewClient(senderID int, apiKey string, h Handler, opts ClientOptions) (*Client, error) {
	host, port := opts.Endpoint.Addr()
	addr := fmt.Sprintf("%s:%d", host, port)
	user := fmt.Sprintf("%d@gcm.googleapis.com", senderID)
	factory := opts.xmppClientFactory()
	client, err := factory.NewXMPPClient(addr, user, apiKey)
	if err != nil {
		return nil, err
	}

	c := &Client{
		host:            host,
		debug:           opts.Debug,
		client:          client,
		pendingMessages: make(map[string]struct{}),
		outMessage:      make(chan struct{}, opts.maxPendMsgs()),
		pong:            make(chan struct{}, 1),
		done:            make(chan struct{}),
	}

	go func() {
		if err := c.listen(h); err != nil {
			_ = atomic.CompareAndSwapInt32(&c.state, StateConnected, StateClosed) || atomic.CompareAndSwapInt32(&c.state, StateDraining, StateClosed)
		}
		close(c.done)
	}()

	return c, nil
}

func (c *Client) trackPendingMsg(id string) bool {
	c.pendingMessagesMu.Lock()
	defer c.pendingMessagesMu.Unlock()
	_, found := c.pendingMessages[id]
	if found {
		return false
	}

	c.pendingMessages[id] = struct{}{}
	return true
}

func (c *Client) untrackPendingMsg(id string) bool {
	c.pendingMessagesMu.Lock()
	defer c.pendingMessagesMu.Unlock()
	_, found := c.pendingMessages[id]
	if !found {
		return false
	}

	delete(c.pendingMessages, id)
	return true
}

// SendData to a destination app identified by regID.
func (c *Client) SendData(ctx context.Context, msgID string, regID string, data interface{}, opts SendOptions) (err error) {
	select {
	case c.outMessage <- struct{}{}:
		c.wg.Add(1)
		defer func() {
			if err != nil {
				<-c.outMessage
				c.wg.Done()
			}
		}()
	case <-ctx.Done():
		return ctx.Err()
	}

	if atomic.LoadInt32(&c.state) != StateConnected {
		return errors.New("gcm-xmpp: not in connected state")
	}

	if ok := c.trackPendingMsg(msgID); !ok {
		return errors.New("gcm-xmpp: duplicate message")
	}

	defer func() {
		if err != nil {
			c.untrackPendingMsg(msgID)
		}
	}()

	msg := message{
		ID:                       msgID,
		To:                       regID,
		Data:                     data,
		DeliveryReceiptRequested: opts.RequestDeliveryReceipt,
		DryRun:                   opts.DryRun,
		TimeToLive:               opts.ttl(),
	}
	stanza, err := buildStanza(msg)
	if err != nil {
		return err
	}

	_, err = c.client.SendOrg(stanza)
	return err
}

// SendMessage to a destination app identified by regID using FCM Message object
func (c *Client) SendMessage(ctx context.Context, msgID string, regID string, notification Notification, data interface{}, opts SendOptions) (err error) {
	select {
	case c.outMessage <- struct{}{}:
		c.wg.Add(1)
		defer func() {
			if err != nil {
				<-c.outMessage
				c.wg.Done()
			}
		}()
	case <-ctx.Done():
		return ctx.Err()
	}

	if atomic.LoadInt32(&c.state) != StateConnected {
		return errors.New("gcm-xmpp: not in connected state")
	}

	if ok := c.trackPendingMsg(msgID); !ok {
		return errors.New("gcm-xmpp: duplicate message")
	}

	defer func() {
		if err != nil {
			c.untrackPendingMsg(msgID)
		}
	}()

	msg := message{
		ID:                       msgID,
		To:                       regID,
		Notification:             notification,
		Data:                     data,
		DeliveryReceiptRequested: opts.RequestDeliveryReceipt,
		DryRun:                   opts.DryRun,
		TimeToLive:               opts.ttl(),
	}
	stanza, err := buildStanza(msg)
	if err != nil {
		return err
	}

	_, err = c.client.SendOrg(stanza)
	return err
}

// Ping server.
func (c *Client) Ping(ctx context.Context) error {
	c.pingMu.Lock()
	defer c.pingMu.Unlock()

	state := atomic.LoadInt32(&c.state)
	if state != StateConnected && state != StateDraining {
		return errors.New("gcm-xmpp: not in connected state")
	}

	// clearing pong
Loop:
	for {
		select {
		case <-c.pong:
		default:
			break Loop
		}
	}

	if err := c.client.PingC2S("", c.host); err != nil {
		return err
	}

	select {
	case <-c.pong:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) sendAck(msgID string, to string) error {
	ack := message{ID: msgID, To: to, Type: "ack"}
	stanza, err := buildStanza(ack)
	if err != nil {
		return err
	}

	_, err = c.client.SendOrg(stanza)
	return err
}

func (c *Client) listen(h Handler) error { // nolint: gocyclo
	for i := 0; ; i++ {
		stanza, err := c.client.Recv()
		if err != nil {
			return err
		}

		switch v := stanza.(type) {
		case xmpp.Chat:
		case xmpp.IQ:
			if v.Type == "result" && v.ID == "c2s1" {
				select {
				case c.pong <- struct{}{}:
				default:
					// full
				}
			}
			continue
		default:
			return fmt.Errorf("gcm-xmpp: unexpected stanza %s", reflect.TypeOf(stanza))
		}

		v := stanza.(xmpp.Chat)
		if len(v.Other) == 0 {
			return errors.New("gcm-xmpp: no data to decode")
		}

		var sm serverMessage
		if err := json.Unmarshal([]byte(v.Other[0]), &sm); err != nil {
			return err
		}

		switch v.Type {
		case "":
			// a response
			switch sm.MessageType {
			case "ack":
				if ok := c.untrackPendingMsg(sm.MessageID); !ok {
					continue
				}

				<-c.outMessage
				_ = h.Handle(c, Ack{From: sm.From, MessageID: sm.MessageID, CanonicalRegistrationID: sm.RegistrationID}) // nolint: gas
				c.wg.Done()
			case "nack":
				if ok := c.untrackPendingMsg(sm.MessageID); !ok {
					continue
				}

				if sm.Error == codeDraining {
					atomic.CompareAndSwapInt32(&c.state, StateConnected, StateDraining)
				}

				<-c.outMessage
				_ = h.Handle(c, Nack{From: sm.From, MessageID: sm.MessageID, Error: sm.Error, ErrorDescription: sm.ErrorDescription}) // nolint: gas
				c.wg.Done()
			default:
				if c.debug {
					log.Printf("Unrecognized message type: %#v\n", sm)
				}
			}
		case "normal":
			// incoming server message
			switch sm.MessageType {
			case "receipt":
				var data receiptData
				if err := json.Unmarshal(sm.Data, &data); err != nil {
					return err
				}

				unix, err := strconv.ParseInt(data.MessageSentTimestamp, 10, 64)
				if err != nil {
					return err
				}

				sentTime := time.Unix(unix/1000, (unix%1000)*1000000)
				if err := h.Handle(c, Receipt{From: data.DeviceRegistrationID, MessageID: data.OriginalMessageID, MessageStatus: data.MessageStatus, SentTime: sentTime}); err != nil {
					continue
				}

				if err := c.sendAck(sm.MessageID, sm.From); err != nil {
					return err
				}
			case "control":
				if sm.ControlType == codeDraining {
					atomic.CompareAndSwapInt32(&c.state, StateConnected, StateDraining)
				}

				_ = h.Handle(c, Control{Type: sm.ControlType}) // nolint: gas
			default:
				if c.debug {
					log.Printf("Unrecognized server message type: %#v\n", sm)
				}
			}
		default:
			if c.debug {
				log.Printf("Unknown type: %#v\n", v)
			}
		}
		if i%1000 == 0 {
			runtime.Gosched()
		}
	}
}

// State of the client.
func (c *Client) State() int32 {
	return atomic.LoadInt32(&c.state)
}

// Done is done channel that will closed if all the resources released.
func (c *Client) Done() <-chan struct{} {
	return c.done
}

// Close the client.
//
// Use the ctx cancelation/deadline wisely. If the cancelation initiateed or
// deadline exceed then the connection will be forced to close without waiting
// the un-ack responses.
func (c *Client) Close(ctx context.Context) error {
	if !(atomic.CompareAndSwapInt32(&c.state, StateConnected, StateClosing) ||
		atomic.CompareAndSwapInt32(&c.state, StateDraining, StateClosing)) {
		return errors.New("gcm-xmpp: not in connected state")
	}

	defer atomic.StoreInt32(&c.state, StateClosed)

	done := waitDone(&c.wg)
	select {
	case <-done:
		return c.client.Close()
	default:
	}

	select {
	case <-done:
		return c.client.Close()
	case <-ctx.Done():
		_ = c.client.Close() // nolint: gas
		return ctx.Err()
	}
}

func waitDone(wg *sync.WaitGroup) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	return done
}

// ClientOptions is the options for the client.
type ClientOptions struct {
	Endpoint           Endpoint          // Used endpoint Default to Prod.
	MaxPendingMessages uint              // Max pending messages. Default to 100.
	Debug              bool              // Enable debug mode. Default to false.
	XMPPClientFactory  XMPPClientFactory // The XMPPClientFactory. Default to RealXMPPClientFactory.
}

func (c ClientOptions) maxPendMsgs() uint {
	if c.MaxPendingMessages == 0 {
		return 100
	}

	return c.MaxPendingMessages
}

func (c *ClientOptions) xmppClientFactory() XMPPClientFactory {
	if c.XMPPClientFactory == nil {
		return DefaultXMPPClientFactory
	}

	return c.XMPPClientFactory
}

// Handler handle incoming message.
// Message can be Ack, Nack, Receipt, Control.
//
// Message handling should not block too long. Long running message handling should be done in another go routine.
//
// All returned error ignored except Receipt. Nil error will send back ack to the server, otherwise no ack (or nack) will be sent.
type Handler interface {
	Handle(src, msg interface{}) error
}

// HandlerFunc the the function adapter for Handler.
type HandlerFunc func(src, msg interface{}) error

// Handle invoke f(msg).
func (f HandlerFunc) Handle(src, msg interface{}) error {
	return f(src, msg)
}

func buildStanza(m message) (string, error) {
	body, err := json.Marshal(m)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf(stanzaFmt, m.ID, string(body)), nil
}

type Notification struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Image string `json:"image"`
	Sound string `json:"sound"`
}

type message struct {
	To                       string       `json:"to"`
	ID                       string       `json:"message_id"`
	Type                     string       `json:"message_type,omitempty"`
	DeliveryReceiptRequested bool         `json:"delivery_receipt_requested,omitempty"`
	DryRun                   bool         `json:"dry_run,omitempty"`
	TimeToLive               uint         `json:"time_to_live,omitempty"`
	Notification             Notification `json:"notification,omitempty"`
	Data                     interface{}  `json:"data"`
}

type serverMessage struct {
	MessageType      string          `json:"message_type"`
	MessageID        string          `json:"message_id"`
	Data             json.RawMessage `json:"data"`
	From             string          `json:"from"`
	RegistrationID   string          `json:"registration_id"`
	Error            string          `json:"error"`
	ErrorDescription string          `json:"error_description"`
	ControlType      string          `json:"control_type"`
}

type receiptData struct {
	MessageStatus        string `json:"message_status"`
	OriginalMessageID    string `json:"original_message_id"`
	DeviceRegistrationID string `json:"device_registration_id"`
	MessageSentTimestamp string `json:"message_sent_timestamp"`
}

// SendOptions is the send options.
type SendOptions struct {
	DryRun                 bool          // Test without actually sending. Default to false.
	RequestDeliveryReceipt bool          // Request for deliverey receipt. Default to false.
	TimeToLive             time.Duration // Time to live. Default to 4 weeks, max up to 4 weeks.
}

func (o SendOptions) ttl() uint {
	ttl := o.TimeToLive
	if ttl >= duration4Weeks {
		return 0
	}

	return uint(ttl.Seconds())
}

// Ack message.
type Ack struct {
	MessageID               string // Original message id.
	From                    string // App registration token.
	CanonicalRegistrationID string // Canonical registration id.
}

// Nack message.
type Nack struct {
	MessageID        string // Original message id.
	From             string // App registration id.
	Error            string // Error code (ex: BAD_REGISTRATION, DEVICE_MESSAGE_RATE_EXCEEDED, INVALID_JSON).
	ErrorDescription string // Error description.
}

// Receipt message.
type Receipt struct {
	MessageStatus string    // Message status (ex: MESSAGE_SENT_TO_DEVICE).
	MessageID     string    // Original message id.
	From          string    // App registration id.
	SentTime      time.Time // Message sent timestamp.
}

// Control message.
type Control struct {
	Type string // Control type, currently only CONNECTION_DRAINING.
}
