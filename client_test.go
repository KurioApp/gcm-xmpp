package xmpp_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"

	goxmpp "github.com/mattn/go-xmpp"

	"github.com/KurioApp/gcm-xmpp/internal/mocks"

	"github.com/KurioApp/gcm-xmpp"
)

type fixture struct {
	t                 *testing.T
	xmppClient        *mocks.XMPPClient
	xmppClientFactory *mocks.XMPPClientFactory
	handler           *mocks.Handler
	client            *xmpp.Client
}

func (f *fixture) tearDown() {
	mock.AssertExpectationsForObjects(f.t,
		f.xmppClient,
		f.xmppClientFactory,
		f.handler)
}

type fixtureOptions struct {
	senderID   int
	apiKey     string
	clientOpts xmpp.ClientOptions
}

func (o fixtureOptions) setup(t *testing.T) *fixture {
	host, port := o.clientOpts.Endpoint.Addr()
	addr := fmt.Sprintf("%s:%d", host, port)
	user := fmt.Sprintf("%d@%s", o.senderID, host)

	xmppClient := new(mocks.XMPPClient)
	xmppClientFactory := new(mocks.XMPPClientFactory)
	handler := new(mocks.Handler)

	xmppClientFactory.On("NewXMPPClient", addr, user, o.apiKey).Return(xmppClient, nil)

	// override the XMPPClientFactory
	o.clientOpts.XMPPClientFactory = xmppClientFactory
	client, err := xmpp.NewClient(o.senderID, o.apiKey, handler, o.clientOpts)
	if err != nil {
		t.Fatal(err)
	}

	return &fixture{
		t:                 t,
		xmppClient:        xmppClient,
		xmppClientFactory: xmppClientFactory,
		handler:           handler,
		client:            client,
	}
}

func TestClient_Ping(t *testing.T) {
	opts := fixtureOptions{
		senderID:   1616,
		apiKey:     "an-api-key",
		clientOpts: xmpp.ClientOptions{},
	}
	fix := opts.setup(t)
	defer fix.tearDown()

	host, _ := opts.clientOpts.Endpoint.Addr()
	fix.xmppClient.On("PingC2S", "", host).Return(nil)
	fix.xmppClient.On("Recv").Return(goxmpp.IQ{Type: "result", ID: "c2s1"}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err := fix.client.Ping(ctx)
	if err != nil {
		t.Fatal(err)
	}
}

func TestClient_Ping_timeout(t *testing.T) {
	opts := fixtureOptions{
		senderID:   1616,
		apiKey:     "an-api-key",
		clientOpts: xmpp.ClientOptions{},
	}
	fix := opts.setup(t)
	defer fix.tearDown()

	host, _ := opts.clientOpts.Endpoint.Addr()
	fix.xmppClient.On("PingC2S", "", host).Return(nil)
	fix.xmppClient.On("Recv").Return(goxmpp.IQ{Type: "result", ID: "c2s1"}, nil).After(1 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err := fix.client.Ping(ctx)
	if got, want := err, context.DeadlineExceeded; got != want {
		t.Fatal("got:", got, "want:", want)
	}
}

func TestClient_SendData_thenClose(t *testing.T) {
	opts := fixtureOptions{
		senderID:   1616,
		apiKey:     "an-api-key",
		clientOpts: xmpp.ClientOptions{},
	}
	fix := opts.setup(t)
	defer fix.tearDown()

	// SendData
	var (
		msgID = "a-msg-id"
		token = "a-token"
		data  = map[string]interface{}{
			"title": "Greet",
			"body":  "Hello World!",
		}
		sendOpts = xmpp.SendOptions{}
	)

	fix.xmppClient.On("SendOrg", mock.AnythingOfType("string")).Return(func(org string) int {
		return len(org)
	}, nil)

	dataSent := make(chan time.Time)
	chat := goxmpp.Chat{
		Other: []string{fmt.Sprintf(`{"from": "%s", "message_id": "%s", "message_type": "ack"}`, token, msgID)},
	}
	fix.xmppClient.On("Recv").Return(chat, nil).WaitUntil(dataSent).Once()

	handled := make(chan struct{})
	fix.handler.On("Handle", fix.client, xmpp.Ack{From: token, MessageID: msgID}).Return(nil).Run(func(args mock.Arguments) {
		close(handled)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err := fix.client.SendData(ctx, msgID, token, data, sendOpts)
	if err != nil {
		t.Fatal(err)
	}

	close(dataSent)

	if !waitUntil(handled, 500*time.Millisecond) {
		t.Fatal("Timeout")
	}

	// Close
	closed, done := make(chan time.Time), make(chan struct{})
	fix.xmppClient.On("Recv").Return(nil, errors.New("closed")).WaitUntil(closed).Run(func(args mock.Arguments) {
		close(done)
	}).Once()
	fix.xmppClient.On("Close").Return(nil)

	closeCtx, cancelClose := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelClose()
	err = fix.client.Close(closeCtx)
	if err != nil {
		t.Fatal(err)
	}

	close(closed)

	if !waitUntil(done, 500*time.Millisecond) {
		t.Fatal("Timeout")
	}
}

func TestClient_Close(t *testing.T) {
	opts := fixtureOptions{
		senderID:   1616,
		apiKey:     "an-api-key",
		clientOpts: xmpp.ClientOptions{},
	}
	fix := opts.setup(t)
	defer fix.tearDown()

	closed, done := make(chan time.Time), make(chan struct{})
	fix.xmppClient.On("Recv").Return(nil, errors.New("closed")).WaitUntil(closed).Run(func(args mock.Arguments) {
		close(done)
	})

	fix.xmppClient.On("Close").Return(nil)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Second)
	defer cancel()
	err := fix.client.Close(ctx)
	if err != nil {
		t.Fatal(err)
	}

	close(closed)

	if !waitUntil(done, 500*time.Millisecond) {
		t.Fatal("Fatal")
	}
}

func waitUntil(c <-chan struct{}, timeout time.Duration) bool {
	select {
	case <-c:
		return true
	case <-time.After(timeout):
		return false
	}
}
