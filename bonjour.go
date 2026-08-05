package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/grandcat/zeroconf"
)

// bonjourEndpoint is a local XMPP server discovered via Bonjour (mDNS/DNS-SD).
type bonjourEndpoint struct {
	Host     string // hostname, e.g. "MyMac.local."
	Port     int
	TXT      []string
	AddrIPv4 []net.IP
	AddrIPv6 []net.IP
}

// bonjourServiceTypes is the DNS-SD service types browsed, in order, to find a
// local XMPP service: the Bonjour IM / serverless-messaging type (_presence._tcp,
// used by Adium and Messages), then the modern client-server _xmpp-client._tcp,
// then the legacy _jabber._tcp.
var bonjourServiceTypes = []string{"_presence._tcp", "_xmpp-client._tcp", "_jabber._tcp"}

// bonjourSettle is how long to collect announcements after the first one, so a
// fully-resolved record is preferred over a partial one.
const bonjourSettle = 2 * time.Second

// bonjourEmptyProbe bounds how long a service type with no matching server is
// probed before moving on, so an absent type doesn't stall discovery.
const bonjourEmptyProbe = 3 * time.Second

// discoverBonjour browses Bonjour for a local XMPP service, trying each service
// type in bonjourServiceTypes (or just service when set) until one responds,
// and returns the first matching endpoint. nameFilter, when non-empty, narrows
// the search to service instances whose name contains it.
func discoverBonjour(ctx context.Context, service, nameFilter string, timeout time.Duration) (*bonjourEndpoint, error) {
	services := bonjourServiceTypes
	if strings.TrimSpace(service) != "" {
		services = []string{strings.TrimSpace(service)}
	}
	probe := timeout
	for i, svc := range services {
		// Probe the (first) configured type for the full timeout; later types
		// only briefly, so an empty type (e.g. no _xmpp-client server, only
		// Bonjour IM) is skipped without stalling the whole discovery.
		if i > 0 && probe > bonjourEmptyProbe {
			probe = bonjourEmptyProbe
		}
		if e, err := browseBonjour(ctx, svc, nameFilter, probe); err == nil {
			return e, nil
		} else if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("no local XMPP service found via Bonjour (mDNS) within %s", timeout)
}

// bonjourInstance is one discovered Bonjour service instance; in serverless
// messaging (_presence._tcp) each instance is a single online user, with a name
// like "crn@mbpro".
type bonjourInstance struct {
	Instance string // e.g. "crn@mbpro"
	Host     string // e.g. "mbpro.local."
	Port     int
}

// discoverBonjourInstances browses Bonjour for the given service type and
// returns every discovered instance (deduplicated by instance name), so tools
// like --discover can list the users currently online. nameFilter, when
// non-empty, keeps only instances whose name contains it.
func discoverBonjourInstances(ctx context.Context, service, nameFilter string, timeout time.Duration) ([]bonjourInstance, error) {
	if strings.TrimSpace(service) == "" {
		service = defaultBonjourService
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		return nil, err
	}
	entries := make(chan *zeroconf.ServiceEntry, 16)
	if err := resolver.Browse(ctx, service, "local", entries); err != nil {
		return nil, err
	}

	seen := map[string]bonjourInstance{}
	for {
		select {
		case e, ok := <-entries:
			if !ok {
				goto done
			}
			if e == nil || e.Instance == "" || (nameFilter != "" && !strings.Contains(unescapeInstance(e.Instance), nameFilter)) {
				continue
			}
			if _, exists := seen[e.Instance]; !exists {
				seen[e.Instance] = bonjourInstance{Instance: e.Instance, Host: e.HostName, Port: e.Port}
			}
		case <-ctx.Done():
			goto done
		}
	}
done:
	if len(seen) == 0 {
		return nil, fmt.Errorf("no %s services found via Bonjour (mDNS) within %s", service, timeout)
	}
	names := make([]string, 0, len(seen))
	for k := range seen {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([]bonjourInstance, 0, len(names))
	for _, k := range names {
		out = append(out, seen[k])
	}
	return out, nil
}

// browseBonjour runs one DNS-SD browse for svc and returns a resolved endpoint.
func browseBonjour(ctx context.Context, svc, nameFilter string, timeout time.Duration) (*bonjourEndpoint, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		return nil, err
	}
	entries := make(chan *zeroconf.ServiceEntry, 8)
	if err := resolver.Browse(ctx, svc, "local", entries); err != nil {
		return nil, err
	}

	var (
		best  *zeroconf.ServiceEntry
		timer *time.Timer
	)
	for {
		var timerC <-chan time.Time
		if timer != nil {
			timerC = timer.C
		}
		select {
		case e, ok := <-entries:
			if !ok {
				if best != nil {
					return endpointFor(best), nil
				}
				return nil, errors.New("browse ended before a server was found")
			}
			if e == nil || (nameFilter != "" && !strings.Contains(unescapeInstance(e.Instance), nameFilter)) {
				continue
			}
			if best == nil || endpointScore(e) > endpointScore(best) {
				best = e
			}
			if nameFilter != "" {
				return endpointFor(best), nil // named service: done
			}
			if timer == nil {
				// First matching entry: give other (fully-resolved) records a
				// moment to arrive before picking a winner.
				timer = time.NewTimer(bonjourSettle)
			}
		case <-timerC:
			if best != nil {
				return endpointFor(best), nil
			}
			timer = nil // no entries yet: keep waiting until the timeout
		case <-ctx.Done():
			if best != nil {
				return endpointFor(best), nil
			}
			return nil, ctx.Err()
		}
	}
}

// endpointFor reduces a browse result to the fields the bridge needs.
func endpointFor(e *zeroconf.ServiceEntry) *bonjourEndpoint {
	return &bonjourEndpoint{
		Host:     e.HostName,
		Port:     e.Port,
		TXT:      append([]string(nil), e.Text...),
		AddrIPv4: append([]net.IP(nil), e.AddrIPv4...),
		AddrIPv6: append([]net.IP(nil), e.AddrIPv6...),
	}
}

// endpointScore ranks a browse result so a fully-resolved record (carrying an
// IP address) is preferred over a name-only one.
func endpointScore(e *zeroconf.ServiceEntry) int {
	s := 0
	if len(e.AddrIPv4) > 0 {
		s += 2
	}
	if len(e.AddrIPv6) > 0 {
		s++
	}
	return s
}

// dialAddr returns the address to dial for a discovered endpoint: the first
// resolved IPv4 address, else the first IPv6 address, else the mDNS hostname
// (which the OS resolves). The caller joins the port with net.JoinHostPort.
func (e *bonjourEndpoint) dialAddr() string {
	if len(e.AddrIPv4) > 0 {
		return e.AddrIPv4[0].String()
	}
	if len(e.AddrIPv6) > 0 {
		return e.AddrIPv6[0].String()
	}
	return strings.TrimSuffix(e.Host, ".")
}
