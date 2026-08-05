package main

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/grandcat/zeroconf"
	"mellium.im/xmpp"
	"mellium.im/xmpp/jid"
	"mellium.im/xmpp/stanza"
	"mellium.im/xmpp/stream"
)

// streamNS is the XML namespace of the stream:stream open/close element.
const streamNS = "http://etherx.jabber.org/streams"

// serverlessDialTimeout bounds each outbound peer dial + stream handshake.
const serverlessDialTimeout = 30 * time.Second

// writeStreamOpen writes an XML declaration and an opening <stream:stream>
// element to conn (to=peer, from=us), the framing XEP-0174 direct connections
// use. mellium's stream package does the same internally, but it's not
// importable, so a custom Negotiator reimplements it with the exported API.
func writeStreamOpen(conn net.Conn, to, from jid.JID, id string) error {
	s := "<?xml version='1.0'?><stream:stream xmlns='" + stanza.NSClient +
		"' xmlns:stream='" + streamNS + "' version='" + stream.DefaultVersion.String() + "'"
	if id != "" {
		s += " id='" + xmlAttrEscape(id) + "'"
	}
	if to.String() != "" {
		s += " to='" + xmlAttrEscape(to.String()) + "'"
	}
	if from.String() != "" {
		s += " from='" + xmlAttrEscape(from.String()) + "'"
	}
	s += ">"
	_, err := io.WriteString(conn, s)
	return err
}

// xmlAttrEscape escapes the characters that would break an XML attribute value.
func xmlAttrEscape(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '&':
			b = append(b, "&amp;"...)
		case '<':
			b = append(b, "&lt;"...)
		case '>':
			b = append(b, "&gt;"...)
		case '"':
			b = append(b, "&quot;"...)
		case '\'':
			b = append(b, "&apos;"...)
		default:
			b = append(b, c)
		}
	}
	return string(b)
}

// expectStreamOpen reads the peer's opening <stream:stream> element from the
// session and populates in with its attributes. It skips leading whitespace,
// surfaces a stream-level <error/>, and is lenient about a missing default
// namespace or version (serverless peers are not always strict).
func expectStreamOpen(session *xmpp.Session, in *stream.Info) error {
	r := session.TokenReader()
	defer r.Close()
	for {
		tok, err := r.Token()
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch {
			case t.Name.Space == streamNS && t.Name.Local == "stream":
				if err := in.FromStartElement(t); err != nil {
					return err
				}
				if in.XMLNS == "" {
					in.XMLNS = stanza.NSClient
				}
				if in.Version == (stream.Version{}) {
					in.Version = stream.DefaultVersion
				}
				return nil
			case t.Name.Space == streamNS && t.Name.Local == "error":
				se := stream.Error{}
				if err := xml.NewTokenDecoder(r).DecodeElement(&se, &t); err != nil {
					return err
				}
				return se
			default:
				return fmt.Errorf("serverless: expected stream open element, got %+v", t.Name)
			}
		case xml.EndElement:
			return io.ErrUnexpectedEOF // peer closed before the handshake completed
		case xml.CharData, xml.Comment, xml.ProcInst, xml.Directive:
			// whitespace / declarations: skip
		}
	}
}

// serverlessInitiator returns a Negotiator that performs the XEP-0174
// initiating side of a direct peer connection: send our stream header, read the
// peer's, and consider the session ready without any feature negotiation
// (there is no server to advertise features, and serverless peers never send a
// <stream:features>).
func serverlessInitiator() xmpp.Negotiator {
	return func(_ context.Context, in, out *stream.Info, session *xmpp.Session, data interface{}) (xmpp.SessionState, io.ReadWriter, interface{}, error) {
		// For initiated connections LocalAddr is us and RemoteAddr is the peer.
		out.XMLNS = stanza.NSClient
		out.Version = stream.DefaultVersion
		if err := writeStreamOpen(session.Conn(), session.RemoteAddr(), session.LocalAddr(), ""); err != nil {
			return 0, nil, data, err
		}
		if err := expectStreamOpen(session, in); err != nil {
			return 0, nil, data, err
		}
		return xmpp.Ready, nil, data, nil
	}
}

// serverlessReceiver returns a Negotiator for the accepting side of a direct
// peer connection: read the initiator's stream header, then reply with ours.
// own is our JID, used as the response stream's "from" when the initiator
// omitted the "to" attribute.
func serverlessReceiver(own jid.JID) xmpp.Negotiator {
	return func(_ context.Context, in, out *stream.Info, session *xmpp.Session, data interface{}) (xmpp.SessionState, io.ReadWriter, interface{}, error) {
		if err := expectStreamOpen(session, in); err != nil {
			return 0, nil, data, err
		}
		peer := in.From
		me := in.To
		if me.String() == "" {
			me = own
		}
		out.XMLNS = stanza.NSClient
		out.Version = stream.DefaultVersion
		if err := writeStreamOpen(session.Conn(), peer, me, newStanzaID()); err != nil {
			return 0, nil, data, err
		}
		return xmpp.Ready, nil, data, nil
	}
}

// connectServerless dials the owner's Bonjour IM endpoint and negotiates a raw
// XEP-0174 peer stream from the initiating side. The returned session is
// bidirectional: the owner's client delivers inbound messages over it too, so
// no separate receive connection is required for replies to flow back.
func connectServerless(ctx context.Context, own, peer jid.JID, endpoint *bonjourEndpoint) (*xmpp.Session, error) {
	dialCtx, cancel := context.WithTimeout(ctx, serverlessDialTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "tcp", net.JoinHostPort(endpoint.dialAddr(), strconv.Itoa(endpoint.Port)))
	if err != nil {
		return nil, fmt.Errorf("dialing peer %s: %w", peer, err)
	}
	session, err := xmpp.NewSession(dialCtx, peer, own, conn, 0, serverlessInitiator())
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("serverless stream with %s: %w", peer, err)
	}
	return session, nil
}

// serverlessAdvertised owns the bot's Bonjour IM presence: the mDNS
// registration of its own _presence._tcp instance and the TCP listener that
// accepts inbound serverless connections from the owner's client.
type serverlessAdvertised struct {
	svc  *zeroconf.Server
	ln   net.Listener
	port int
	own  jid.JID
	txt  []string // base TXT keys (txtvers, port, phsh) excluding status
}

// advertiseAndListen registers the bot's _presence._tcp instance (the JID as
// instance name, plus the XEP-0174 TXT keys) and starts listening for inbound
// serverless connections. It prefers the standard port 5298, falling back to an
// ephemeral port when that is taken (another IM client may already own it); the
// actual port is advertised in the TXT record.
func advertiseAndListen(ctx context.Context, own jid.JID, avatarHash string) (*serverlessAdvertised, error) {
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(serverlessPort))
	if err != nil {
		ln, err = net.Listen("tcp", ":0")
		if err != nil {
			return nil, fmt.Errorf("listening for serverless connections: %w", err)
		}
	}
	port := ln.Addr().(*net.TCPAddr).Port

	txt := []string{"txtvers=1", "port=" + strconv.Itoa(port), "status=avail"}
	if avatarHash != "" {
		txt = append(txt, "phsh="+avatarHash)
	}
	svc, err := zeroconf.Register(escapeInstance(own.String()), "_presence._tcp", "local", port, txt, nil)
	if err != nil {
		ln.Close()
		return nil, fmt.Errorf("registering Bonjour presence: %w", err)
	}
	base := append([]string(nil), txt...)
	return &serverlessAdvertised{svc: svc, ln: ln, port: port, own: own, txt: base}, nil
}

// shutdown unregisters the mDNS presence and closes the listener.
func (a *serverlessAdvertised) shutdown() {
	a.svc.Shutdown()
	_ = a.ln.Close()
}

// setStatus updates the "status=" TXT key on the live mDNS registration, so the
// owner's client shows the bot's availability (avail/dnd/away) without any
// in-stream presence round trip. Other keys (txtvers, port, phsh) are kept.
func (a *serverlessAdvertised) setStatus(status string) {
	txt := append([]string(nil), a.txt...)
	txt = append(txt, "status="+status)
	a.svc.SetText(txt)
}

// serve accepts inbound serverless connections and runs a received peer session
// for each, dispatching stanzas to handler, until ctx is canceled.
func (a *serverlessAdvertised) serve(ctx context.Context, handler xmpp.Handler) {
	for {
		conn, err := a.ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		go func(c net.Conn) {
			session, err := xmpp.ReceiveSession(ctx, c, 0, serverlessReceiver(a.own))
			if err != nil {
				_ = c.Close()
				return
			}
			_ = session.Serve(handler)
			_ = session.Close()
		}(conn)
	}
}
