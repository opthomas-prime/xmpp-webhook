package main

import (
	"context"
	"crypto/tls"
	"encoding/xml"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/tmsmr/xmpp-webhook/parser"
	"mellium.im/sasl"
	"mellium.im/xmlstream"
	"mellium.im/xmpp"
	"mellium.im/xmpp/dial"
	"mellium.im/xmpp/jid"
	"mellium.im/xmpp/stanza"
)

func panicOnErr(err error) {
	if err != nil {
		panic(err)
	}
}

type MessageBody struct {
	stanza.Message
	Body string `xml:"body"`
}

func initXMPP(address jid.JID, pass string, skipTLSVerify bool, useXMPPS bool) (*xmpp.Session, error) {
	tlsConfig := tls.Config{InsecureSkipVerify: skipTLSVerify}
	var dialer dial.Dialer
	// only use the tls config for the dialer if necessary
	if skipTLSVerify {
		dialer = dial.Dialer{NoTLS: !useXMPPS, TLSConfig: &tlsConfig}
	} else {
		dialer = dial.Dialer{NoTLS: !useXMPPS}
	}
	conn, err := dialer.Dial(context.TODO(), "tcp", address)
	if err != nil {
		return nil, err
	}
	// we need the domain in the tls config if we want to verify the cert
	if !skipTLSVerify {
		tlsConfig.ServerName = address.Domainpart()
	}
	return xmpp.NewSession(
		context.TODO(),
		address.Domain(),
		address,
		conn,
		0,
		xmpp.NewNegotiator(xmpp.StreamConfig{Features: func(_ *xmpp.Session, f ...xmpp.StreamFeature) []xmpp.StreamFeature {
			if f != nil {
				return f
			}
			return []xmpp.StreamFeature{
				xmpp.BindResource(),
				xmpp.StartTLS(&tlsConfig),
				xmpp.SASL("", pass, sasl.ScramSha256Plus, sasl.ScramSha256, sasl.ScramSha1Plus, sasl.ScramSha1, sasl.Plain),
			}
		}}),
	)
}

func closeXMPP(session *xmpp.Session) {
	_ = session.Close()
	_ = session.Conn().Close()
}

func main() {
	// get xmpp credentials, message recipients
	xi := os.Getenv("XMPP_ID")
	xp := os.Getenv("XMPP_PASS")
	xr := os.Getenv("XMPP_RECIPIENTS")

	// get tls settings from env
	_, skipTLSVerify := os.LookupEnv("XMPP_SKIP_VERIFY")
	_, useXMPPS := os.LookupEnv("XMPP_OVER_TLS")

	// get listen address
	listenAddress := os.Getenv("XMPP_WEBHOOK_LISTEN_ADDRESS")
	if len(listenAddress) == 0 {
		listenAddress = ":4321"
	}

	// check if xmpp credentials and recipient list are supplied
	if xi == "" || xp == "" || xr == "" {
		log.Fatal("XMPP_ID, XMPP_PASS or XMPP_RECIPIENTS not set")
	}

	myjid, err := jid.Parse(xi)
	panicOnErr(err)

	// connect to xmpp server
	xmppSession, err := initXMPP(myjid, xp, skipTLSVerify, useXMPPS)
	panicOnErr(err)
	defer closeXMPP(xmppSession)

	// send initial presence
	panicOnErr(xmppSession.Send(context.TODO(), stanza.Presence{Type: stanza.AvailablePresence}.Wrap(nil)))

	// listen for messages and echo them
	go func() {
		err = xmppSession.Serve(xmpp.HandlerFunc(func(t xmlstream.TokenReadEncoder, start *xml.StartElement) error {
			d := xml.NewTokenDecoder(t)
			// ignore elements that aren't messages
			if start.Name.Local != "message" {
				return nil
			}

			// parse message into struct
			msg := MessageBody{}
			err = d.DecodeElement(&msg, start)
			if err != nil && err != io.EOF {
				return nil
			}

			// ignore empty messages and stanzas that aren't messages
			if msg.Body == "" || msg.Type != stanza.ChatMessage {
				return nil
			}

			// create reply with identical contents
			reply := MessageBody{
				Message: stanza.Message{
					To:   msg.From.Bare(),
					From: myjid,
					Type: stanza.ChatMessage,
				},
				Body: msg.Body,
			}

			// try to send reply, ignore errors
			_ = t.Encode(reply)
			return nil
		}))
		panicOnErr(err)
	}()

	// create chan for messages (webhooks -> xmpp)
	messages := make(chan alertMessage)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// wait for messages from the webhooks and send them to all recipients
	go func() {
		for m := range messages {
			// use recipients configured in ENV
			recipients := strings.Split(xr, ",")
			if m.recipients != nil {
				// use recipients from request parameter
				recipients = m.recipients
			}
			for _, r := range recipients {
				recipient, err := jid.Parse(r)
				if err != nil {
					continue
				}
				// try to send message, ignore errors
				_ = xmppSession.Encode(ctx, MessageBody{
					Message: stanza.Message{
						To:   recipient,
						From: myjid,
						Type: stanza.ChatMessage,
					},
					Body: m.message,
				})
			}
		}
	}()

	// initialize handlers with associated parser functions
	http.Handle("/grafana", newMessageHandler(messages, parser.GrafanaParserFunc))
	http.Handle("/slack", newMessageHandler(messages, parser.SlackParserFunc))
	http.Handle("/alertmanager", newMessageHandler(messages, parser.AlertmanagerParserFunc))

	// listen for requests
	_ = http.ListenAndServe(listenAddress, nil)
}
