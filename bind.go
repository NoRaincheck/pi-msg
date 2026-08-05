package main

import (
	"context"
	"encoding/xml"
	"io"

	"mellium.im/xmlstream"
	"mellium.im/xmpp"
	"mellium.im/xmpp/jid"
	"mellium.im/xmpp/stanza"
	"mellium.im/xmpp/stream"
)

// bindNS is the resource-binding namespace (RFC 6120 §7).
const bindNS = "urn:ietf:params:xml:ns:xmpp-bind"

// bindPayload is the <bind/> payload carried by resource-binding IQs.
type bindPayload struct {
	Resource string  `xml:"resource,omitempty"`
	JID      jid.JID `xml:"jid,omitempty"`
}

// bindIQ is a resource-binding IQ.
type bindIQ struct {
	stanza.IQ
	Bind bindPayload   `xml:"urn:ietf:params:xml:ns:xmpp-bind bind,omitempty"`
	Err  *stanza.Error `xml:"error,omitempty"`
}

// TokenReader renders the bind request as an XML token stream: an IQ carrying
// a <bind><resource>…</resource></bind> payload.
func (biq *bindIQ) TokenReader() xml.TokenReader {
	return biq.Wrap(xmlstream.Wrap(
		xmlstream.Wrap(
			xmlstream.ReaderFunc(func() (xml.Token, error) {
				return xml.CharData(biq.Bind.Resource), io.EOF
			}),
			xml.StartElement{Name: xml.Name{Local: "resource"}},
		),
		xml.StartElement{Name: xml.Name{Local: "bind", Space: bindNS}},
	))
}

// WriteXML writes the bind request to w.
func (biq *bindIQ) WriteXML(w xmlstream.TokenWriter) (n int, err error) {
	return xmlstream.Copy(w, biq.TokenReader())
}

// bindNoAuth returns a resource-binding stream feature that, unlike the stock
// BindResource, does not require the authenticated (Authn) session state. It
// exists for anonymous Bonjour sessions where SASL is skipped entirely: the
// server advertises binding, and negotiation would otherwise fail with
// "features advertised out of order" because no feature matches.
func bindNoAuth() xmpp.StreamFeature {
	return xmpp.StreamFeature{
		Name:       xml.Name{Space: bindNS, Local: "bind"},
		Prohibited: xmpp.Ready,
		Parse: func(_ context.Context, d *xml.Decoder, start *xml.StartElement) (bool, interface{}, error) {
			parsed := struct {
				XMLName xml.Name `xml:"urn:ietf:params:xml:ns:xmpp-bind bind"`
			}{}
			return true, nil, d.DecodeElement(&parsed, start)
		},
		Negotiate: func(ctx context.Context, session *xmpp.Session, _ interface{}) (xmpp.SessionState, io.ReadWriter, error) {
			return negotiateBindNoAuth(ctx, session)
		},
	}
}

// negotiateBindNoAuth performs the client side of resource binding: request a
// resource, wait for the server's IQ result, and adopt the returned full JID.
func negotiateBindNoAuth(ctx context.Context, session *xmpp.Session) (xmpp.SessionState, io.ReadWriter, error) {
	var mask xmpp.SessionState
	w := session.TokenWriter()
	defer w.Close()
	r := session.TokenReader()
	defer r.Close()
	d := xml.NewTokenDecoder(r)

	reqID := newStanzaID()
	req := &bindIQ{
		IQ: stanza.IQ{
			XMLName: xml.Name{Space: stanza.NSClient, Local: "iq"},
			ID:      reqID,
			Type:    stanza.SetIQ,
		},
		Bind: bindPayload{Resource: session.LocalAddr().Resourcepart()},
	}
	if _, err := req.WriteXML(w); err != nil {
		return mask, nil, err
	}
	if err := w.Flush(); err != nil {
		return mask, nil, err
	}

	tok, err := d.Token()
	if err != nil {
		return mask, nil, err
	}
	start, ok := tok.(xml.StartElement)
	if !ok {
		return mask, nil, stream.BadFormat
	}
	resp := bindIQ{}
	switch start.Name {
	case xml.Name{Space: stanza.NSClient, Local: "iq"}:
		if err := d.DecodeElement(&resp, &start); err != nil {
			return mask, nil, err
		}
	default:
		return mask, nil, stream.BadFormat
	}
	switch {
	case resp.ID != reqID:
		return mask, nil, stream.UndefinedCondition
	case resp.Type == stanza.ResultIQ:
		session.UpdateAddr(resp.Bind.JID)
	case resp.Type == stanza.ErrorIQ:
		if resp.Err != nil {
			return mask, nil, resp.Err
		}
		return mask, nil, stream.UndefinedCondition
	default:
		return mask, nil, stanza.Error{Condition: stanza.BadRequest}
	}
	return xmpp.Ready, nil, nil
}
