package writebackcoh

import (
	"fmt"
	"os"
	"strings"

	"github.com/sarchlab/akita/v4/sim"
)

// deadlockDumpPath is the shared file every component appends its
// post-mortem state to when the engine stops (deadlock or completion).
const deadlockDumpPath = "/root/deadlock_dump.txt"

func l2CanSendState(p sim.Port) string {
	if p == nil {
		return "nil"
	}
	if p.CanSend() {
		return "ok"
	}
	return "FULL"
}

func l2HasIncoming(p sim.Port) bool {
	if p == nil {
		return false
	}
	return p.PeekIncoming() != nil
}

func appendDeadlockDump(s string) {
	f, err := os.OpenFile(deadlockDumpPath,
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(s)
}

// DumpDeadlockState writes this L2 cache's writeBuffer eviction state to the
// shared deadlock dump file. Registered as an engine OnStop hook so the dump
// is RTM-independent (works even when the monitor HTTP server is unresponsive
// after the engine halts).
func (c *Comp) DumpDeadlockState() {
	wb := c.writeBuffer
	if wb == nil {
		return
	}

	// Only dump caches that actually have stuck remote evictions, to keep
	// the file focused on the deadlock participants.
	if wb.numRemoteInflightEviction == 0 && len(wb.inflightEviction) == 0 {
		return
	}

	var sb strings.Builder
	// [ORIGIN-SPLIT] report own/peer sub-counts so the deadlock dump shows
	// which origin saturated its remote-inflict sub-budget.
	fmt.Fprintf(&sb, "[L2 %s] remoteInfl=%d (own=%d peer=%d) pendRemote=%d (own=%d peer=%d) localInfl=%d deferredFlush=%d (own=%d peer=%d) pendWriteDoneR=%d pendDataReadyR=%d writeDoneRecv=%d noMatch=%d | remoteBotEgress=%s remoteBotHasIn=%v\n",
		c.name, wb.numRemoteInflightEviction, wb.numRemoteInflEvictOwn, wb.numRemoteInflEvictPeer,
		wb.numPendingRemoteEvictionsOwn+wb.numPendingRemoteEvictionsPeer,
		wb.numPendingRemoteEvictionsOwn, wb.numPendingRemoteEvictionsPeer,
		wb.numLocalInflightEviction, len(wb.deferredFlushOwn)+len(wb.deferredFlushPeer),
		len(wb.deferredFlushOwn), len(wb.deferredFlushPeer),
		len(wb.pendingWriteDoneRemote), len(wb.pendingDataReadyRemote),
		wb.writeDoneReceivedCount, wb.writeDoneNoMatchCount,
		l2CanSendState(c.remoteBottomPort), l2HasIncoming(c.remoteBottomPort))

	for i, t := range wb.inflightEviction {
		if i >= 16 {
			fmt.Fprintf(&sb, "  ...(%d more inflightEviction)\n", len(wb.inflightEviction)-16)
			break
		}
		id := "<nil>"
		if t.evictionWriteReq != nil {
			id = t.evictionWriteReq.ID
		}
		fmt.Fprintf(&sb, "  inflEvict[%d] reqID=%s addr=%x home=%d toLocal=%v\n",
			i, id, t.evictingAddr, t.evictingAddr/(4*1024*1024*1024), t.evictionToLocal)
	}

	appendDeadlockDump(sb.String())
}
