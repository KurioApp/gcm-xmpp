package xmpp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"reflect"
	"strconv"
	"sync"
	"time"

	xmpp "github.com/mattn/go-xmpp"
)

// Endpoint is the endpoint type. Options: Prod and PreProd
type Endpoint int

const (
	// Prod is production endpoint.
	Prod Endpoint = iota

	// PreProd is pre-production endpoint.
	PreProd
)

const (
	prodHost = "gcm.googleapis.com"
	prodPort = 5235

	preProdHost = "gcm-preprod.googleapis.com"
	preProdPort = 5236
)

const stanzaFmt = `<message id="%s"><gcm xmlns="google:mobile:data">%v</gcm></message>`
const duration4Weeks = 4 * 7 * 24 * time.Hour

// Client of the GCM.
type Client struct {
	host  string
	debug bool

	xc       *xmpp.Client
	messagec chan struct{}

	messagesMu sync.RWMutex
	messages   map[string]struct{}

	pingMu sync.Mutex
	pongc  chan struct{}
}

func (c *Client) trackMsg(id string) bool {
	c.messagesMu.Lock()
	defer c.messagesMu.Unlock()
	_, found := c.messages[id]
	if found {
		return false
	}

	c.messages[id] = struct{}{}
	return true
}

func (c *Client) untrackMsg(id string) bool {
	c.messagesMu.Lock()
	defer c.messagesMu.Unlock()
	_, found := c.messages[id]
	if !found {
		return false
	}

	delete(c.messages, id)
	return true
}

// SendData to a destination token. The ctx used for flow control, might wait for pending messages.
func (c *Client) SendData(ctx context.Context, msgID string, token string, data interface{}, opts SendOptions) error {
	select {
	case c.messagec <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}

	if ok := c.trackMsg(msgID); !ok {
		return errors.New("duplicate message")
	}

	msg := message{
		ID:   msgID,
		To:   token,
		Data: data,
		DeliveryReceiptRequested: opts.RequestDeliveryReceipt,
		DryRun:     opts.DryRun,
		TimeToLive: opts.ttl(),
	}
	stanza, err := buildStanza(msg)
	if err != nil {
		return err
	}

	_, err = c.xc.SendOrg(stanza)
	return err
}

// Ping server. Better to always put timeout on the context.
func (c *Client) Ping(ctx context.Context) error {
	c.pingMu.Lock()
	defer c.pingMu.Unlock()

	// clear pongc
	for i := 0; i < len(c.pongc); i++ {
		<-c.pongc
	}

	if err := c.xc.PingC2S("", c.host); err != nil {
		return err
	}

	select {
	case <-c.pongc:
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

	_, err = c.xc.SendOrg(stanza)
	return err
}

// Listen for response or incoming message from server.
func (c *Client) Listen(h Handler) error {
	for {
		stanza, err := c.xc.Recv()
		if err != nil {
			return err
		}

		switch v := stanza.(type) {
		case xmpp.Chat:
		case xmpp.IQ:
			if v.Type == "result" && v.ID == "c2s1" && len(c.pongc) == 0 {
				c.pongc <- struct{}{}
			}
			continue
		default:
			return fmt.Errorf("unexpected stanza: %s", reflect.TypeOf(stanza))
		}

		v := stanza.(xmpp.Chat)
		if len(v.Other) == 0 {
			return errors.New("no data to decode")
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
				if ok := c.untrackMsg(sm.MessageID); !ok {
					continue
				}

				<-c.messagec
				_ = h.Handle(Ack{From: sm.From, MessageID: sm.MessageID})
			case "nack":
				if ok := c.untrackMsg(sm.MessageID); !ok {
					continue
				}

				<-c.messagec
				_ = h.Handle(Nack{From: sm.From, MessageID: sm.MessageID, Error: sm.Error, ErrorDescription: sm.ErrorDescription})
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
				if err := h.Handle(Receipt{From: data.DeviceRegistrationID, MessageID: data.OriginalMessageID, MessageStatus: data.MessageStatus, SentTime: sentTime}); err != nil {
					continue
				}

				if err := c.sendAck(sm.MessageID, sm.From); err != nil {
					return err
				}
			case "control":
				_ = h.Handle(Control{Type: sm.ControlType})
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
	}
}

// Close the client.
func (c *Client) Close() error {
	return c.xc.Close()
}

// NewClient constructs new client.
func NewClient(senderID int, apiKey string, opts ClientOptions) (*Client, error) {
	host, port := opts.endpoint()
	addr := fmt.Sprintf("%s:%d", host, port)
	user := fmt.Sprintf("%d@%s", senderID, host)
	client, err := xmpp.NewClient(addr, user, apiKey, false)
	if err != nil {
		return nil, err
	}

	return &Client{
		host:     host,
		debug:    opts.Debug,
		xc:       client,
		messages: make(map[string]struct{}),
		messagec: make(chan struct{}, opts.maxPendMsgs()),
		pongc:    make(chan struct{}, 1),
	}, nil
}

// ClientOptions is the options for the client.
type ClientOptions struct {
	Endpoint           Endpoint // Used endpoint Default to Prod
	MaxPendingMessages uint     // Max pending messages. Default to 100
	Debug              bool     // Enable debug mode. Default to false.
}

func (c ClientOptions) endpoint() (string, int) {
	if c.Endpoint == PreProd {
		return preProdHost, preProdPort
	}

	return prodHost, prodPort
}

func (c ClientOptions) maxPendMsgs() uint {
	if c.MaxPendingMessages == 0 {
		return 100
	}

	return c.MaxPendingMessages
}

// Handler handle incoming message.
// Message can me Ack, Nack, Receipt, Control.
// Message handling should not block too long. Long running message handling should be done in another go routine.
// All returned error ignored except Receipt. Nil error will send back ack to the server.
type Handler interface {
	Handle(msg interface{}) error
}

// HandlerFunc the the function adapter for Handler.
type HandlerFunc func(msg interface{}) error

// Handle invoke f(msg).
func (f HandlerFunc) Handle(msg interface{}) error {
	return f(msg)
}

func buildStanza(m message) (string, error) {
	body, err := json.Marshal(m)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf(stanzaFmt, m.ID, string(body)), nil
}

type message struct {
	To                       string      `json:"to"`
	ID                       string      `json:"message_id"`
	Type                     string      `json:"message_type,omitempty"`
	DeliveryReceiptRequested bool        `json:"delivery_receipt_requested,omitempty"`
	DryRun                   bool        `json:"dry_run,omitempty"`
	TimeToLive               uint        `json:"time_to_live,omitempty"`
	Data                     interface{} `json:"data,omitempty"`
}

type serverMessage struct {
	MessageType      string          `json:"message_type"`
	MessageID        string          `json:"message_id"`
	Data             json.RawMessage `json:"data"`
	From             string          `json:"from"`
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
	MessageID string // Original message id
	From      string // App registration token
}

// Nack message.
type Nack struct {
	MessageID        string // Original message id
	From             string // App registration token
	Error            string // Error code (ex: BAD_REGISTRATION, DEVICE_MESSAGE_RATE_EXCEEDED, INVALID_JSON)
	ErrorDescription string // Error description
}

// Receipt message.
type Receipt struct {
	MessageStatus string    // Message status (ex: MESSAGE_SENT_TO_DEVICE)
	MessageID     string    // Original message id
	From          string    // App registration token
	SentTime      time.Time // Sent timestamp
}

// Control message.
type Control struct {
	Type string // Control type, currently only CONNECTION_DRAINING
}
