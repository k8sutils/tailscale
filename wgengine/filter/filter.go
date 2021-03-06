// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package filter is a stateful packet filter.
package filter

import (
	"fmt"
	"sync"
	"time"

	"github.com/golang/groupcache/lru"
	"golang.org/x/time/rate"
	"inet.af/netaddr"
	"tailscale.com/net/packet"
	"tailscale.com/types/logger"
)

// Filter is a stateful packet filter.
type Filter struct {
	logf logger.Logf
	// local4 and local6 are the lists of IP prefixes that we know
	// to be "local" to this node. All packets coming in over
	// tailscale must have a destination within local4 or local6,
	// regardless of the policy filter below. Zero values reject
	// all incoming traffic.
	local4 []net4
	local6 []net6
	// matches4 and matches6 are lists of match->action rules
	// applied to all packets arriving over tailscale
	// tunnels. Matches are checked in order, and processing stops
	// at the first matching rule. The default policy if no rules
	// match is to drop the packet.
	matches4 matches4
	matches6 matches6
	// state is the connection tracking state attached to this
	// filter. It is used to allow incoming traffic that is a response
	// to an outbound connection that this node made, even if those
	// incoming packets don't get accepted by matches above.
	state4 *filterState
	state6 *filterState
}

// tuple4 is a 4-tuple of source and destination IPv4 and port. It's
// used as a lookup key in filterState.
type tuple4 struct {
	SrcIP   packet.IP4
	DstIP   packet.IP4
	SrcPort uint16
	DstPort uint16
}

// tuple6 is a 4-tuple of source and destination IPv6 and port. It's
// used as a lookup key in filterState.
type tuple6 struct {
	SrcIP   packet.IP6
	DstIP   packet.IP6
	SrcPort uint16
	DstPort uint16
}

// filterState is a state cache of past seen packets.
type filterState struct {
	mu  sync.Mutex
	lru *lru.Cache // of tuple4 or tuple6
}

// lruMax is the size of the LRU cache in filterState.
const lruMax = 512

// Response is a verdict from the packet filter.
type Response int

const (
	Drop      Response = iota // do not continue processing packet.
	Accept                    // continue processing packet.
	noVerdict                 // no verdict yet, continue running filter
)

func (r Response) String() string {
	switch r {
	case Drop:
		return "Drop"
	case Accept:
		return "Accept"
	case noVerdict:
		return "noVerdict"
	default:
		return "???"
	}
}

// RunFlags controls the filter's debug log verbosity at runtime.
type RunFlags int

const (
	LogDrops       RunFlags = 1 << iota // write dropped packet info to logf
	LogAccepts                          // write accepted packet info to logf
	HexdumpDrops                        // print packet hexdump when logging drops
	HexdumpAccepts                      // print packet hexdump when logging accepts
)

// NewAllowAllForTest returns a packet filter that accepts
// everything. Use in tests only, as it permits some kinds of spoofing
// attacks to reach the OS network stack.
func NewAllowAllForTest(logf logger.Logf) *Filter {
	any4 := netaddr.IPPrefix{IP: netaddr.IPv4(0, 0, 0, 0), Bits: 0}
	any6 := netaddr.IPPrefix{IP: netaddr.IPFrom16([16]byte{}), Bits: 0}
	ms := []Match{
		{
			Srcs: []netaddr.IPPrefix{any4},
			Dsts: []NetPortRange{
				{
					Net: any4,
					Ports: PortRange{
						First: 0,
						Last:  65535,
					},
				},
			},
		},
		{
			Srcs: []netaddr.IPPrefix{any6},
			Dsts: []NetPortRange{
				{
					Net: any6,
					Ports: PortRange{
						First: 0,
						Last:  65535,
					},
				},
			},
		},
	}

	return New(ms, []netaddr.IPPrefix{any4, any6}, nil, logf)
}

// NewAllowNone returns a packet filter that rejects everything.
func NewAllowNone(logf logger.Logf) *Filter {
	return New(nil, nil, nil, logf)
}

// New creates a new packet filter. The filter enforces that incoming
// packets must be destined to an IP in localNets, and must be allowed
// by matches. If shareStateWith is non-nil, the returned filter
// shares state with the previous one, to enable changing rules at
// runtime without breaking existing stateful flows.
func New(matches []Match, localNets []netaddr.IPPrefix, shareStateWith *Filter, logf logger.Logf) *Filter {
	var state4, state6 *filterState
	if shareStateWith != nil {
		state4 = shareStateWith.state4
		state6 = shareStateWith.state6
	} else {
		state4 = &filterState{
			lru: lru.New(lruMax),
		}
		state6 = &filterState{
			lru: lru.New(lruMax),
		}
	}
	f := &Filter{
		logf:     logf,
		matches4: newMatches4(matches),
		matches6: newMatches6(matches),
		local4:   nets4FromIPPrefixes(localNets),
		local6:   nets6FromIPPrefixes(localNets),
		state4:   state4,
		state6:   state6,
	}
	return f
}

func maybeHexdump(flag RunFlags, b []byte) string {
	if flag == 0 {
		return ""
	}
	return packet.Hexdump(b) + "\n"
}

// TODO(apenwarr): use a bigger bucket for specifically TCP SYN accept logging?
//   Logging is a quick way to record every newly opened TCP connection, but
//   we have to be cautious about flooding the logs vs letting people use
//   flood protection to hide their traffic. We could use a rate limiter in
//   the actual *filter* for SYN accepts, perhaps.
var acceptBucket = rate.NewLimiter(rate.Every(10*time.Second), 3)
var dropBucket = rate.NewLimiter(rate.Every(5*time.Second), 10)

func (f *Filter) logRateLimit(runflags RunFlags, q *packet.Parsed, dir direction, r Response, why string) {
	var verdict string

	if r == Drop && omitDropLogging(q, dir) {
		return
	}

	if r == Drop && (runflags&LogDrops) != 0 && dropBucket.Allow() {
		verdict = "Drop"
		runflags &= HexdumpDrops
	} else if r == Accept && (runflags&LogAccepts) != 0 && acceptBucket.Allow() {
		verdict = "Accept"
		runflags &= HexdumpAccepts
	}

	// Note: it is crucial that q.String() be called only if {accept,drop}Bucket.Allow() passes,
	// since it causes an allocation.
	if verdict != "" {
		b := q.Buffer()
		f.logf("%s: %s %d %s\n%s", verdict, q.String(), len(b), why, maybeHexdump(runflags, b))
	}
}

// dummyPacket is a 20-byte slice of garbage, to pass the filter
// pre-check when evaluating synthesized packets.
var dummyPacket = []byte{
	0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
	0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
}

// CheckTCP determines whether TCP traffic from srcIP to dstIP:dstPort
// is allowed.
func (f *Filter) CheckTCP(srcIP, dstIP netaddr.IP, dstPort uint16) Response {
	pkt := &packet.Parsed{}
	pkt.Decode(dummyPacket) // initialize private fields
	switch {
	case (srcIP.Is4() && dstIP.Is6()) || (srcIP.Is6() && srcIP.Is4()):
		// Mistmatched address families, no filters will
		// match.
		return Drop
	case srcIP.Is4():
		pkt.IPVersion = 4
		pkt.SrcIP4 = packet.IP4FromNetaddr(srcIP)
		pkt.DstIP4 = packet.IP4FromNetaddr(dstIP)
	case srcIP.Is6():
		pkt.IPVersion = 6
		pkt.SrcIP6 = packet.IP6FromNetaddr(srcIP)
		pkt.DstIP6 = packet.IP6FromNetaddr(dstIP)
	default:
		panic("unreachable")
	}
	pkt.IPProto = packet.TCP
	pkt.TCPFlags = packet.TCPSyn
	pkt.SrcPort = 0
	pkt.DstPort = dstPort

	return f.RunIn(pkt, 0)
}

// RunIn determines whether this node is allowed to receive q from a
// Tailscale peer.
func (f *Filter) RunIn(q *packet.Parsed, rf RunFlags) Response {
	dir := in
	r := f.pre(q, rf, dir)
	if r == Accept || r == Drop {
		// already logged
		return r
	}

	var why string
	switch q.IPVersion {
	case 4:
		r, why = f.runIn4(q)
	case 6:
		r, why = f.runIn6(q)
	default:
		r, why = Drop, "not-ip"
	}
	f.logRateLimit(rf, q, dir, r, why)
	return r
}

// RunOut determines whether this node is allowed to send q to a
// Tailscale peer.
func (f *Filter) RunOut(q *packet.Parsed, rf RunFlags) Response {
	dir := out
	r := f.pre(q, rf, dir)
	if r == Drop || r == Accept {
		// already logged
		return r
	}
	r, why := f.runOut(q)
	f.logRateLimit(rf, q, dir, r, why)
	return r
}

func (f *Filter) runIn4(q *packet.Parsed) (r Response, why string) {
	// A compromised peer could try to send us packets for
	// destinations we didn't explicitly advertise. This check is to
	// prevent that.
	if !ip4InList(q.DstIP4, f.local4) {
		return Drop, "destination not allowed"
	}

	switch q.IPProto {
	case packet.ICMPv4:
		if q.IsEchoResponse() || q.IsError() {
			// ICMP responses are allowed.
			// TODO(apenwarr): consider using conntrack state.
			//  We could choose to reject all packets that aren't
			//  related to an existing ICMP-Echo, TCP, or UDP
			//  session.
			return Accept, "icmp response ok"
		} else if f.matches4.matchIPsOnly(q) {
			// If any port is open to an IP, allow ICMP to it.
			return Accept, "icmp ok"
		}
	case packet.TCP:
		// For TCP, we want to allow *outgoing* connections,
		// which means we want to allow return packets on those
		// connections. To make this restriction work, we need to
		// allow non-SYN packets (continuation of an existing session)
		// to arrive. This should be okay since a new incoming session
		// can't be initiated without first sending a SYN.
		// It happens to also be much faster.
		// TODO(apenwarr): Skip the rest of decoding in this path?
		if q.IPProto == packet.TCP && !q.IsTCPSyn() {
			return Accept, "tcp non-syn"
		}
		if f.matches4.match(q) {
			return Accept, "tcp ok"
		}
	case packet.UDP:
		t := tuple4{q.SrcIP4, q.DstIP4, q.SrcPort, q.DstPort}

		f.state4.mu.Lock()
		_, ok := f.state4.lru.Get(t)
		f.state4.mu.Unlock()

		if ok {
			return Accept, "udp cached"
		}
		if f.matches4.match(q) {
			return Accept, "udp ok"
		}
	default:
		return Drop, "Unknown proto"
	}
	return Drop, "no rules matched"
}

func (f *Filter) runIn6(q *packet.Parsed) (r Response, why string) {
	// A compromised peer could try to send us packets for
	// destinations we didn't explicitly advertise. This check is to
	// prevent that.
	if !ip6InList(q.DstIP6, f.local6) {
		return Drop, "destination not allowed"
	}

	switch q.IPProto {
	case packet.ICMPv6:
		if q.IsEchoResponse() || q.IsError() {
			// ICMP responses are allowed.
			// TODO(apenwarr): consider using conntrack state.
			//  We could choose to reject all packets that aren't
			//  related to an existing ICMP-Echo, TCP, or UDP
			//  session.
			return Accept, "icmp response ok"
		} else if f.matches6.matchIPsOnly(q) {
			// If any port is open to an IP, allow ICMP to it.
			return Accept, "icmp ok"
		}
	case packet.TCP:
		// For TCP, we want to allow *outgoing* connections,
		// which means we want to allow return packets on those
		// connections. To make this restriction work, we need to
		// allow non-SYN packets (continuation of an existing session)
		// to arrive. This should be okay since a new incoming session
		// can't be initiated without first sending a SYN.
		// It happens to also be much faster.
		// TODO(apenwarr): Skip the rest of decoding in this path?
		if q.IPProto == packet.TCP && !q.IsTCPSyn() {
			return Accept, "tcp non-syn"
		}
		if f.matches6.match(q) {
			return Accept, "tcp ok"
		}
	case packet.UDP:
		t := tuple6{q.SrcIP6, q.DstIP6, q.SrcPort, q.DstPort}

		f.state6.mu.Lock()
		_, ok := f.state6.lru.Get(t)
		f.state6.mu.Unlock()

		if ok {
			return Accept, "udp cached"
		}
		if f.matches6.match(q) {
			return Accept, "udp ok"
		}
	default:
		return Drop, "Unknown proto"
	}
	return Drop, "no rules matched"
}

// runIn runs the output-specific part of the filter logic.
func (f *Filter) runOut(q *packet.Parsed) (r Response, why string) {
	if q.IPProto != packet.UDP {
		return Accept, "ok out"
	}

	switch q.IPVersion {
	case 4:
		t := tuple4{q.DstIP4, q.SrcIP4, q.DstPort, q.SrcPort}
		var ti interface{} = t // allocate once, rather than twice inside mutex
		f.state4.mu.Lock()
		f.state4.lru.Add(ti, ti)
		f.state4.mu.Unlock()
	case 6:
		t := tuple6{q.DstIP6, q.SrcIP6, q.DstPort, q.SrcPort}
		var ti interface{} = t // allocate once, rather than twice inside mutex
		f.state6.mu.Lock()
		f.state6.lru.Add(ti, ti)
		f.state6.mu.Unlock()
	}
	return Accept, "ok out"
}

// direction is whether a packet was flowing in to this machine, or
// flowing out.
type direction int

const (
	in  direction = iota // from Tailscale peer to local machine
	out                  // from local machine to Tailscale peer
)

func (d direction) String() string {
	switch d {
	case in:
		return "in"
	case out:
		return "out"
	default:
		return fmt.Sprintf("[??dir=%d]", int(d))
	}
}

// pre runs the direction-agnostic filter logic. dir is only used for
// logging.
func (f *Filter) pre(q *packet.Parsed, rf RunFlags, dir direction) Response {
	if len(q.Buffer()) == 0 {
		// wireguard keepalive packet, always permit.
		return Accept
	}
	if len(q.Buffer()) < 20 {
		f.logRateLimit(rf, q, dir, Drop, "too short")
		return Drop
	}

	switch q.IPVersion {
	case 4:
		if q.DstIP4.IsMulticast() {
			f.logRateLimit(rf, q, dir, Drop, "multicast")
			return Drop
		}
		if q.DstIP4.IsMostLinkLocalUnicast() {
			f.logRateLimit(rf, q, dir, Drop, "link-local-unicast")
			return Drop
		}
	case 6:
		if q.DstIP6.IsMulticast() {
			f.logRateLimit(rf, q, dir, Drop, "multicast")
			return Drop
		}
		if q.DstIP6.IsLinkLocalUnicast() {
			f.logRateLimit(rf, q, dir, Drop, "link-local-unicast")
			return Drop
		}
	}

	switch q.IPProto {
	case packet.Unknown:
		// Unknown packets are dangerous; always drop them.
		f.logRateLimit(rf, q, dir, Drop, "unknown")
		return Drop
	case packet.Fragment:
		// Fragments after the first always need to be passed through.
		// Very small fragments are considered Junk by Parsed.
		f.logRateLimit(rf, q, dir, Accept, "fragment")
		return Accept
	}

	return noVerdict
}

// omitDropLogging reports whether packet p, which has already been
// deemed a packet to Drop, should bypass the [rate-limited] logging.
// We don't want to log scary & spammy reject warnings for packets
// that are totally normal, like IPv6 route announcements.
func omitDropLogging(p *packet.Parsed, dir direction) bool {
	if dir != out {
		return false
	}

	switch p.IPVersion {
	case 4:
		return p.DstIP4.IsMulticast() || p.DstIP4.IsMostLinkLocalUnicast() || p.IPProto == packet.IGMP
	case 6:
		return p.DstIP6.IsMulticast() || p.DstIP6.IsLinkLocalUnicast()
	default:
		return false
	}
}
