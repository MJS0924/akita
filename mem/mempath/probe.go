// Package mempath provides a lightweight, flag-gated, timing-neutral probe
// that rides along a memory request as it descends the GPU memory hierarchy
// (AddressTranslator -> L1 -> Coherence Directory -> L2 -> DRAM, or via RDMA
// to a remote GPU) and is reflected back on the response. Each component
// stamps a (Location, Event, Time) record at the point it already classifies
// a hit/miss, so that the originating AddressTranslator can reconstruct the
// full path, the location where the access was ultimately served, and the
// per-component latency contribution.
//
// The probe is pure observation: it never feeds back into any simulation
// decision and its presence does not change message TrafficBytes, so a run
// with the probe enabled produces byte-identical simulated timing to a run
// with it disabled. When Enabled is false, no probe is ever allocated and
// every instrumentation site is a single nil/bool check.
package mempath

import "github.com/sarchlab/akita/v4/sim"

// Enabled is the global on/off switch. The mgpusim runner sets it from the
// -mem-latency-trace flag before the platform is built. When false, the
// AddressTranslator never allocates a Probe and every downstream site short
// circuits on a nil PathProbe.
var Enabled bool

// Collect is the global sink, set by the runner to its collector's Add method.
// The originating AddressTranslator calls it exactly once per completed
// request with the finished probe and the total AT-observed latency.
var Collect func(p *Probe, total sim.VTimeInSec)

// Event labels stamped by the components. Kept as constants so the producers
// (cache/dir/dram/rdma code) and the classifier agree on the spelling.
const (
	EvATReqOut = "AT.reqOut"
	EvATRspIn  = "AT.rspIn"

	// Component-entry / response-arrival stamps. Combined with the
	// determination stamps below they isolate each component's own latency
	// (entry -> determination) from the inter-component link/queueing
	// latency (prev-determination -> next-entry).
	EvL1In    = "L1.in"    // request arrived at L1 (from AT)
	EvL1RspIn = "L1.rspIn" // response arrived back at L1 (from Directory)
	EvL2In    = "L2.in"    // request arrived at L2 (from Directory)
	EvL2RspIn = "L2.rspIn" // fill response arrived at L2 (from DRAM/RDMA)

	EvL1ReadHit      = "L1.read-hit"
	EvL1WriteHit     = "L1.write-hit"
	EvL1ReadMiss     = "L1.read-miss"
	EvL1WriteMiss    = "L1.write-miss"
	EvL1ReadMSHRHit  = "L1.read-mshr-hit"
	EvL1WriteMSHRHit = "L1.write-mshr-hit"

	EvDirFwdLocal = "Dir.fwdLocal"
	EvDirFwdRDMA  = "Dir.fwdRDMA"

	EvL2Hit       = "L2.hit"
	EvL2Miss      = "L2.miss"
	EvL2Fill      = "L2.fill"
	EvL2PeerServe = "L2.peerServe"

	EvDRAM = "DRAM"

	EvRDMAOut = "RDMA.out"
	EvRDMAIn  = "RDMA.in"
)

// Hit-class labels returned by Classify.
const (
	ClassL1         = "L1"
	ClassL2Local    = "L2_local"
	ClassRemoteL2   = "remote_L2"
	ClassDRAMLocal  = "DRAM_local"
	ClassDRAMRemote = "DRAM_remote"
	ClassRemoteDir  = "remote_Dir"
	ClassUnknown    = "unknown"
)

// Stamp is one (Location, Event, Time) record appended by a component.
type Stamp struct {
	Location string        // the stamping component's name
	Event    string        // a label from the Ev* constants
	Time     sim.VTimeInSec // simulation time of the stamp
}

// Probe accumulates the ordered stamps of a single memory request. It is
// heap-allocated by the AddressTranslator and shared by pointer along the
// request's descent and its response's ascent. A probe is single-owner along
// its path (only one in-flight component touches it at a time), so appending
// needs no lock; only the shared Collect sink is synchronized.
type Probe struct {
	OriginDevice uint64  // GPU id of the originating AddressTranslator
	OriginUnit   string  // name of the originating AddressTranslator
	Stamps       []Stamp // ordered path records (Stamps[0] is AT.reqOut)
	HitClass     string  // filled in by Classify at completion
	Coalesced    bool    // true if served by another request's in-flight fill
}

// Stamp appends a record. Safe to call on a nil receiver (no-op) so callers
// can write p.Stamp(...) without a separate nil check after a nil-guarded
// fetch.
func (p *Probe) Stamp(location, event string, t sim.VTimeInSec) {
	if p == nil {
		return
	}
	p.Stamps = append(p.Stamps, Stamp{Location: location, Event: event, Time: t})
}

// InheritPath copies the below-L1 "served location" stamps (Directory, L2,
// DRAM, RDMA) from src into dst and marks dst as coalesced. It is used when a
// request is served by another request's in-flight fill (L1 coalescing or an
// MSHR hit): dst keeps its own AT.reqOut as Stamps[0] (so its total latency
// stays correct) but gains src's served-location stamps so it can be
// classified into the right hit-location bucket. dst's own AT/L1 stamps are
// preserved; the coalesced flag excludes dst from the per-component segment
// breakdown (its intermediate timestamps are not its own). No-op on nil or
// self.
func InheritPath(dst, src *Probe) {
	if dst == nil || src == nil || dst == src {
		return
	}
	dst.Coalesced = true
	for _, s := range src.Stamps {
		switch s.Event {
		case EvATReqOut, EvATRspIn,
			EvL1ReadHit, EvL1WriteHit, EvL1ReadMiss, EvL1WriteMiss,
			EvL1ReadMSHRHit, EvL1WriteMSHRHit:
			continue
		}
		dst.Stamps = append(dst.Stamps, s)
	}
}

// has reports whether the probe contains a stamp with the given event.
func (p *Probe) has(event string) bool {
	for i := range p.Stamps {
		if p.Stamps[i].Event == event {
			return true
		}
	}
	return false
}

// hasL2 reports whether the request reached any L2 (hit/miss/fill/peerServe).
func (p *Probe) hasL2() bool {
	for i := range p.Stamps {
		switch p.Stamps[i].Event {
		case EvL2Hit, EvL2Miss, EvL2Fill, EvL2PeerServe:
			return true
		}
	}
	return false
}

// Classify determines where the access was ultimately served from the ordered
// stamp sequence. The deepest reached level wins; the presence of RDMA.out
// flips a local classification to its remote counterpart.
func Classify(p *Probe) string {
	if p == nil || len(p.Stamps) == 0 {
		return ClassUnknown
	}

	remote := p.has(EvRDMAOut)

	switch {
	case p.has(EvDRAM):
		if remote {
			return ClassDRAMRemote
		}
		return ClassDRAMLocal
	case p.has(EvL2PeerServe):
		return ClassRemoteL2
	case p.has(EvL2Hit):
		if remote {
			return ClassRemoteL2
		}
		return ClassL2Local
	case p.has(EvDirFwdRDMA) && remote:
		return ClassRemoteDir
	case p.has(EvL1ReadHit) || p.has(EvL1WriteHit) ||
		p.has(EvL1ReadMSHRHit) || p.has(EvL1WriteMSHRHit):
		if !p.hasL2() {
			return ClassL1
		}
		return ClassUnknown
	default:
		return ClassUnknown
	}
}

// TotalLatency returns the time from the first stamp (AT.reqOut) to the last
// stamp (AT.rspIn). Returns 0 if fewer than two stamps exist.
func (p *Probe) TotalLatency() sim.VTimeInSec {
	if p == nil || len(p.Stamps) < 2 {
		return 0
	}
	return p.Stamps[len(p.Stamps)-1].Time - p.Stamps[0].Time
}
