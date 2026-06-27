package writearound

import "github.com/sarchlab/akita/v4/mem/mempath"

// This file wires the mem-latency path tracer (mempath) into the L1
// write-around cache. All helpers short-circuit when the tracer is disabled
// (mempath.Enabled == false) so they add no allocation and cannot affect
// simulated timing. The probe travels on the original (pre-coalesce) request,
// rides the coalesced fetch down to the directory/L2/DRAM, and is reflected
// back on the response; coalesced secondary requests inherit the served
// location from the representative fetch so each is classified correctly.

// probe returns the path probe carried by a per-request transaction's original
// request (read or write), or nil.
func (t *transaction) probe() *mempath.Probe {
	if t.read != nil {
		return t.read.PathProbe
	}
	if t.write != nil {
		return t.write.PathProbe
	}
	return nil
}

// stampPreCoalesce stamps the given L1 event on every original (pre-coalesce)
// request's probe of a coalesced transaction.
func (c *Comp) stampPreCoalesce(trans *transaction, event string) {
	if !mempath.Enabled {
		return
	}
	now := c.CurrentTime()
	for _, pc := range trans.preCoalesceTransactions {
		pc.probe().Stamp(c.name, event, now)
	}
}

// representativeProbe returns the probe of the first pre-coalesce request,
// which carries the descent path for a coalesced miss/fetch.
func representativeProbe(trans *transaction) *mempath.Probe {
	if !mempath.Enabled || len(trans.preCoalesceTransactions) == 0 {
		return nil
	}
	return trans.preCoalesceTransactions[0].probe()
}

// inheritFanout propagates the served-location stamps from the descent probe
// to every coalesced secondary request of a finished coalesced transaction.
func inheritFanout(trans *transaction, descent *mempath.Probe) {
	if !mempath.Enabled || descent == nil {
		return
	}
	for _, pc := range trans.preCoalesceTransactions {
		mempath.InheritPath(pc.probe(), descent)
	}
}
