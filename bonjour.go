package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/grandcat/zeroconf"
)

// bonjourEndpoint is a local Bonjour IM peer (XEP-0174 serverless messaging)
// discovered via mDNS/DNS-SD.
type bonjourEndpoint struct {
	Host     string // hostname, e.g. "MyMac.local."
	Port     int
	TXT      []string
	AddrIPv4 []net.IP
	AddrIPv6 []net.IP
}

// serverlessPort is the default port for XEP-0174 serverless messaging. It is
// used when neither the SRV record nor the TXT "port=" key specifies one.
const serverlessPort = 5298

// bonjourServiceTypes is the DNS-SD service type browsed to find a Bonjour IM
// peer: _presence._tcp, the serverless-messaging type used by Adium and
// Messages, where each online user advertises an instance like "you@my-mac".
// XEP-0174 has no other type.
var bonjourServiceTypes = []string{"_presence._tcp"}

// bonjourSettle is how long to collect announcements after the first one, so a
// fully-resolved record is preferred over a partial one.
const bonjourSettle = 2 * time.Second

// discoverBonjour browses Bonjour for the Bonjour IM instance belonging to
// target (a bare JID, e.g. "you@mbpro") and returns its endpoint, trying each
// service type in bonjourServiceTypes (or just service when set) until one
// responds. nameFilter, when non-empty, further narrows the search to service
// instances whose name contains it.
func discoverBonjour(ctx context.Context, service, target, nameFilter string, timeout time.Duration) (*bonjourEndpoint, error) {
	services := bonjourServiceTypes
	if strings.TrimSpace(service) != "" {
		services = []string{strings.TrimSpace(service)}
	}
	for _, svc := range services {
		if e, err := browseBonjour(ctx, svc, target, nameFilter, timeout); err == nil {
			return e, nil
		} else if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("no Bonjour IM user matching %q found via mDNS within %s", target, timeout)
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

// browseBonjour runs one DNS-SD browse for svc and returns a resolved endpoint
// for the instance matching target (a bare JID), or any instance when target is
// empty (used by --discover).
func browseBonjour(ctx context.Context, svc, target, nameFilter string, timeout time.Duration) (*bonjourEndpoint, error) {
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

	target = strings.ToLower(strings.TrimSpace(target))
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
				return nil, errors.New("browse ended before a matching instance was found")
			}
			if e == nil || !matchesInstance(e, target, nameFilter) {
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

// matchesInstance reports whether a browse entry belongs to the target JID (a
// bare JID, matched case-insensitively against the unescaped instance name)
// and, when nameFilter is set, also contains it.
func matchesInstance(e *zeroconf.ServiceEntry, target, nameFilter string) bool {
	name := strings.ToLower(unescapeInstance(e.Instance))
	if target != "" && name != target {
		return false
	}
	if nameFilter != "" && !strings.Contains(name, strings.ToLower(nameFilter)) {
		return false
	}
	return true
}

// endpointFor reduces a browse result to the fields the bridge needs. The
// port comes from the TXT "port=" key when present (XEP-0174 §4.2), else the
// SRV record, else the XEP-0174 default.
func endpointFor(e *zeroconf.ServiceEntry) *bonjourEndpoint {
	port := e.Port
	if p, ok := txtPort(e.Text); ok {
		port = p
	}
	if port == 0 {
		port = serverlessPort
	}
	return &bonjourEndpoint{
		Host:     e.HostName,
		Port:     port,
		TXT:      append([]string(nil), e.Text...),
		AddrIPv4: append([]net.IP(nil), e.AddrIPv4...),
		AddrIPv6: append([]net.IP(nil), e.AddrIPv6...),
	}
}

// txtPort returns the value of the "port=" key in a DNS-SD TXT record
// (XEP-0174 §4.2) and whether it was present and valid.
func txtPort(txt []string) (int, bool) {
	for _, kv := range txt {
		k, v, ok := strings.Cut(kv, "=")
		if ok && strings.EqualFold(strings.TrimSpace(k), "port") {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
				return n, true
			}
			return 0, false
		}
	}
	return 0, false
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
