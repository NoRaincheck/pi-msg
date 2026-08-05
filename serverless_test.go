package main

import (
	"context"
	"encoding/xml"
	"net"
	"strings"
	"testing"
	"time"

	"mellium.im/xmlstream"
	"mellium.im/xmpp"
	"mellium.im/xmpp/jid"
	"mellium.im/xmpp/stanza"
)

// TestServerlessRoundTrip drives a full XEP-0174-style direct stream between a
// receiver side (our listener, serverlessReceiver) and an initiator side (the
// owner's client, serverlessInitiator) over a real TCP socket: stream open
// exchange, then a chat message delivered to the receive handler.
func TestServerlessRoundTrip(t *testing.T) {
	own, err := jid.Parse("pi@mbpro")
	if err != nil {
		t.Fatal(err)
	}
	peer, err := jid.Parse("zach@mbpro")
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	received := make(chan string, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Receiver: accept one connection and run a peer session that forwards the
	// first message body.
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		session, err := xmpp.ReceiveSession(ctx, conn, 0, serverlessReceiver(own))
		if err != nil {
			t.Errorf("ReceiveSession: %v", err)
			return
		}
		h := xmpp.HandlerFunc(func(t xmlstream.TokenReadEncoder, start *xml.StartElement) error {
			toks, err := xmlstream.ReadAll(t)
			if err != nil {
				return err
			}
			if start.Name.Local == "message" {
				received <- childText(toks, "body")
			}
			return nil
		})
		_ = session.Serve(h)
		_ = session.Close()
	}()

	// Initiator: dial and run the serverless negotiator.
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	session, err := xmpp.NewSession(ctx, peer, own, conn, 0, serverlessInitiator())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer session.Close()

	// Send a chat message to the peer.
	toJID, _ := jid.Parse("zach@mbpro")
	msg := struct {
		stanza.Message
		Body string `xml:"body"`
	}{
		Message: stanza.Message{ID: "m-1", To: toJID, Type: stanza.ChatMessage},
		Body:    "hello serverless",
	}
	if err := session.Encode(ctx, msg); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	select {
	case got := <-received:
		if got != "hello serverless" {
			t.Errorf("received body = %q, want 'hello serverless'", got)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for the message to arrive")
	}
}

// TestExpectStreamOpenRejectsNonStream verifies expectStreamOpen fails cleanly
// when the peer sends something that is not a stream open element.
func TestExpectStreamOpenRejectsNonStream(t *testing.T) {
	own, _ := jid.Parse("pi@mbpro")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	errCh := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			errCh <- err
			return
		}
		_, err = xmpp.ReceiveSession(context.Background(), conn, 0, serverlessReceiver(own))
		errCh <- err
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Send a plain element with the wrong namespace instead of <stream:stream>.
	if _, err := conn.Write([]byte("<notastream xmlns='urn:example'/>")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		if err == nil {
			t.Error("expected an error from the receiver, got nil")
		} else if !strings.Contains(err.Error(), "expected stream open element") {
			t.Errorf("unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the receiver to fail")
	}
}
