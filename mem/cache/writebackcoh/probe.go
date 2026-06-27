package writebackcoh

import "github.com/sarchlab/akita/v4/mem/mempath"

// This file wires the mem-latency path tracer (mempath) into the L2
// write-back coherent cache. Helpers short-circuit when the tracer is disabled
// so they add no allocation and cannot affect simulated timing. The probe
// arrives on the original request (forwarded down from the directory), rides
// the demand fetch to DRAM/RDMA, and is reflected back on the response;
// MSHR-coalesced secondaries inherit the served location from the fetch.

// probe returns the path probe carried by a transaction's original access
// request (the read/write that arrived from the directory), or nil.
func (t *transaction) probe() *mempath.Probe {
	switch {
	case t.read != nil:
		return t.read.PathProbe
	case t.write != nil:
		return t.write.PathProbe
	}
	return nil
}

// stampProbe stamps an L2 event onto the transaction's original request probe.
func (c *Comp) stampProbe(t *transaction, event string) {
	if !mempath.Enabled {
		return
	}
	t.probe().Stamp(c.name, event, c.CurrentTime())
}

// l2ServedEvent returns the stamp event for a served (hit) transaction: a
// local hit for own-L1 traffic, or a peer-serve for peer-incoming traffic so
// the originating GPU classifies it as a remote-L2 hit.
func l2ServedEvent(fromLocal bool) string {
	if fromLocal {
		return mempath.EvL2Hit
	}
	return mempath.EvL2PeerServe
}
