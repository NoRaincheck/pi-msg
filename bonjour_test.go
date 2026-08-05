package main

import (
	"encoding/xml"
	"net"
	"strings"
	"testing"

	"github.com/grandcat/zeroconf"
	"mellium.im/xmlstream"
	"mellium.im/xmpp/stanza"
)

func TestDialAddr(t *testing.T) {
	cases := []struct {
		name string
		e    *bonjourEndpoint
		want string
	}{
		{
			name: "prefers IPv4",
			e: &bonjourEndpoint{
				Host:     "MyMac.local.",
				AddrIPv4: []net.IP{net.ParseIP("192.168.1.10")},
				AddrIPv6: []net.IP{net.ParseIP("fe80::1")},
			},
			want: "192.168.1.10",
		},
		{
			name: "falls back to IPv6",
			e: &bonjourEndpoint{
				Host:     "MyMac.local.",
				AddrIPv6: []net.IP{net.ParseIP("fe80::1")},
			},
			want: "fe80::1",
		},
		{
			name: "falls back to hostname, trailing dot stripped",
			e:    &bonjourEndpoint{Host: "MyMac.local."},
			want: "MyMac.local",
		},
		{
			name: "hostname without trailing dot",
			e:    &bonjourEndpoint{Host: "mymac.local"},
			want: "mymac.local",
		},
	}
	for _, c := range cases {
		if got := c.e.dialAddr(); got != c.want {
			t.Errorf("%s: dialAddr() = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestEndpointScorePrefersResolved(t *testing.T) {
	nameOnly := &zeroconf.ServiceEntry{HostName: "mymac.local."}
	withV6 := &zeroconf.ServiceEntry{HostName: "mymac.local.", AddrIPv6: []net.IP{net.ParseIP("fe80::1")}}
	withV4 := &zeroconf.ServiceEntry{HostName: "mymac.local.", AddrIPv4: []net.IP{net.ParseIP("192.168.1.10")}}
	withBoth := &zeroconf.ServiceEntry{
		HostName: "mymac.local.",
		AddrIPv4: []net.IP{net.ParseIP("192.168.1.10")},
		AddrIPv6: []net.IP{net.ParseIP("fe80::1")},
	}
	if endpointScore(nameOnly) >= endpointScore(withV6) {
		t.Error("name-only entry should score below one with an IPv6 address")
	}
	if endpointScore(withV6) >= endpointScore(withV4) {
		t.Error("IPv6-only entry should score below one with an IPv4 address")
	}
	if endpointScore(withV4) >= endpointScore(withBoth) {
		t.Error("IPv4-only entry should score below one with both address families")
	}
}

func TestEndpointForCopies(t *testing.T) {
	src := &zeroconf.ServiceEntry{
		HostName: "mymac.local.",
		Port:     5222,
		Text:     []string{"v=1", "tls=true"},
		AddrIPv4: []net.IP{net.ParseIP("192.168.1.10")},
	}
	e := endpointFor(src)
	if e.Host != "mymac.local." || e.Port != 5222 {
		t.Errorf("endpointFor = %+v, want host/port mymac.local./5222", e)
	}
	if strings.Join(e.TXT, ",") != "v=1,tls=true" {
		t.Errorf("TXT = %v, want [v=1 tls=true]", e.TXT)
	}
	// Mutating the original must not affect the copy.
	src.Text[0] = "changed"
	if e.TXT[0] != "v=1" {
		t.Errorf("endpointFor should copy the TXT slice, got %v", e.TXT)
	}
}

func TestUnescapeInstance(t *testing.T) {
	cases := map[string]string{
		`crn\@mbpro`:  `crn@mbpro`,
		"crn@mbpro":   "crn@mbpro",
		`nrc\@mb-air`: `nrc@mb-air`,
	}
	for in, want := range cases {
		if got := unescapeInstance(in); got != want {
			t.Errorf("unescapeInstance(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBindNoAuthRequestXML(t *testing.T) {
	req := &bindIQ{
		IQ:   stanza.IQ{XMLName: xml.Name{Space: stanza.NSClient, Local: "iq"}, ID: "req-1", Type: stanza.SetIQ},
		Bind: bindPayload{Resource: "pi-msg"},
	}
	var buf strings.Builder
	enc := xml.NewEncoder(&buf)
	if _, err := xmlstream.Copy(enc, req.TokenReader()); err != nil {
		t.Fatalf("copy: %v", err)
	}
	enc.Flush()
	got := buf.String()
	for _, want := range []string{
		`<iq`,
		`xmlns="` + stanza.NSClient + `"`,
		`id="req-1"`,
		`type="set"`,
		`<bind xmlns="` + bindNS + `"`,
		`<resource>pi-msg</resource>`,
		`</bind>`,
		`</iq>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("bind IQ = %q, missing %q", got, want)
		}
	}
}
