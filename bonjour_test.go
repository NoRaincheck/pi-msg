package main

import (
	"net"
	"strings"
	"testing"

	"github.com/grandcat/zeroconf"
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

func TestEscapeInstance(t *testing.T) {
	cases := map[string]string{
		"pi@mbpro":  `pi\@mbpro`,
		"crn@mbpro": `crn\@mbpro`,
		"a@b.c/d":   `a\@b\.c/d`,
		`x@y:z,w`:   `x\@y:z\,w`,
		`a\@b`:      `a\\\@b`,
		"plain":     "plain",
	}
	for in, want := range cases {
		if got := escapeInstance(in); got != want {
			t.Errorf("escapeInstance(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMatchesInstance(t *testing.T) {
	entry := func(instance string) *zeroconf.ServiceEntry {
		return &zeroconf.ServiceEntry{ServiceRecord: zeroconf.ServiceRecord{Instance: instance}}
	}
	cases := []struct {
		instance, target, filter string
		want                     bool
	}{
		{`pi\@mbpro`, "pi@mbpro", "", true},
		{`Pi\@MBPRO`, "pi@mbpro", "", true},
		{`pi\@mbpro`, "pi@mbpro", "pi", true},
		{`pi\@mbpro`, "", "mbpro", true},
		{`pi\@mbpro`, "zach@mbpro", "", false},
		{`pi\@macbook`, "pi@mbpro", "", false},
		{`pi\@mbpro`, "pi@mbpro", "macbook", false},
	}
	for _, c := range cases {
		got := matchesInstance(entry(c.instance), c.target, c.filter)
		if got != c.want {
			t.Errorf("matchesInstance(%q, %q, %q) = %v, want %v", c.instance, c.target, c.filter, got, c.want)
		}
	}
}

func TestTxtPort(t *testing.T) {
	// XEP-0174: a "port=" TXT key overrides the SRV record's port.
	if p, ok := txtPort([]string{"txtvers=1", "port=5298", "status=avail"}); !ok || p != 5298 {
		t.Errorf("txtPort with port key = (%d, %v), want (5298, true)", p, ok)
	}
	// Missing key → not ok, caller falls back to the SRV port.
	if _, ok := txtPort([]string{"txtvers=1", "status=avail"}); ok {
		t.Error("txtPort without port key should report not-found")
	}
	// Garbage port value → not ok.
	if _, ok := txtPort([]string{"port=notaport"}); ok {
		t.Error("txtPort with garbage port should report not-found")
	}
	// Zero / negative port → not ok.
	if _, ok := txtPort([]string{"port=0"}); ok {
		t.Error("txtPort with port=0 should report not-found")
	}
}

func TestEndpointForPrefersTxtPort(t *testing.T) {
	src := &zeroconf.ServiceEntry{
		HostName: "mymac.local.",
		Port:     5222,
		Text:     []string{"txtvers=1", "port=5298"},
	}
	e := endpointFor(src)
	if e.Port != 5298 {
		t.Errorf("endpointFor port = %d, want 5298 (TXT override)", e.Port)
	}
}
