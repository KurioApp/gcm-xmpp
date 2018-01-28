package xmpp

import (
	"github.com/mattn/go-xmpp"
)

// XMPPClient is the XMPP Client. Extracted interface from xmpp.Client.
type XMPPClient interface { // nolint: golint
	SendOrg(org string) (n int, err error)
	PingC2S(jid, server string) error
	Recv() (stanza interface{}, err error)
	Close() error
}

// XMPPClientFactory creates XMPPClient.
type XMPPClientFactory interface { // nolint: golint
	NewXMPPClient(host, user, passwd string) (XMPPClient, error)
}

// RealXMPPClientFactory is the factory for real implementation of xmpp.Client
type RealXMPPClientFactory struct {
	Debug bool
}

// NewXMPPClient will create new XMPPClient.
func (f RealXMPPClientFactory) NewXMPPClient(host, user, passwd string) (XMPPClient, error) {
	return xmpp.NewClient(host, user, passwd, f.Debug)
}
