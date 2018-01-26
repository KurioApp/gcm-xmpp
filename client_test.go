package xmpp_test

import (
	"context"
	"fmt"
	"time"

	"github.com/KurioApp/gcm-xmpp"
)

func ExampleClient() {
	var (
		senderID = 8832496
		apiKey   = "some-api-key"
		opts     = xmpp.ClientOptions{}
	)

	handler := xmpp.HandlerFunc(func(src interface{}, msg interface{}) error {
		switch v := msg.(type) {
		case xmpp.Ack:
			// TODO: handle ack
			fmt.Println("Ack for ", v.MessageID)
			return nil
		case xmpp.Nack:
			// TODO: handle nack
			fmt.Println("Nack for ", v.MessageID)
			return nil
		case xmpp.Receipt:
			// TODO: handle delivery receipt
			fmt.Println("Delivery receipt for ", v.MessageID, "status:", v.MessageStatus)
			return nil
		case xmpp.Control:
			// TODO: handle control message (ex: draining connection)
			return nil
		default:
			// TODO: unknown message
			return nil
		}
	})

	client, err := xmpp.NewClient(senderID, apiKey, handler, opts)
	if err != nil {
		// TODO: handle error
		return
	}

	_ = client // TODO: client.Close() when client no longer used
}

func ExampleClient_SendData() {
	var client *xmpp.Client
	// TODO: assign client

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	msgID := "some-unique-msg-id"
	to := "destination-token"
	opts := xmpp.SendOptions{}
	data := map[string]interface{}{
		"text": "Hello World!",
	}

	if err := client.SendData(ctx, msgID, to, data, opts); err != nil {
		// TODO: handle error
	}
}

func ExampleClient_SendData_struct() {
	type Notification struct {
		Text string `json:"text"`
	}

	var client *xmpp.Client
	// TODO: assign client

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	msgID := "some-unique-msg-id"
	to := "destination-token"
	opts := xmpp.SendOptions{}
	data := Notification{
		Text: "Hello World!",
	}

	if err := client.SendData(ctx, msgID, to, data, opts); err != nil {
		// TODO: handle error
	}
}
