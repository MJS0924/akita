package optdirectory

import (
	"fmt"
	"strings"

	"github.com/sarchlab/akita/v4/sim"
)

func odPortState(p sim.Port) string {
	if p == nil {
		return "nil"
	}
	send := "ok"
	if !p.CanSend() {
		send = "FULL"
	}
	in := "-"
	if p.PeekIncoming() != nil {
		in = "hasIn"
	}
	return send + "/" + in
}

// DumpDeadlockState prints this directory's stuck invalidation state to STDOUT
// when the engine halts (deadlock or normal completion). For every waiting
// invalidation it prints the ADDRESS and exactly which peer/L2 ports it still
// awaits an InvRsp from (awaitingInvRsp). Grep one address across every GPU's
// dump (and the writebackcoh L2 dump) to see how far that invalidation was
// delivered and where it stalled:
//
//	GPU[A].CohDir invToOutRemote addr=X awaitingInvRsp=[GPU[B]...]   (sent to peer B, waiting)
//	GPU[B].CohDir invToBottom    addr=X                              (forwarded to local L2, waiting)
//	[L2 GPU[B]...] inflEvict addr=X                                  (L2 stuck on the dirty writeback)
func (c *Comp) DumpDeadlockState() {
	bs := c.bottomSender
	if bs == nil {
		return
	}
	nOwn := len(bs.inflightInvToOutsideOwn)
	nRem := len(bs.inflightInvToOutsideRemote)
	nBot := len(bs.inflightInvToBottom)
	if nOwn == 0 && nRem == 0 && nBot == 0 {
		return
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "\n[DEADLOCK-DUMP %s] inflInvToOutside=%d (own=%d remote=%d) inflInvToBottom=%d | RDMAPort=%s RDMAInvRspPort=%s\n",
		c.name, nOwn+nRem, nOwn, nRem, nBot,
		odPortState(c.RDMAPort), odPortState(c.RDMAInvRspPort))

	dumpOut := func(label string, list []*transaction) {
		for i, t := range list {
			if i >= 12 {
				fmt.Fprintf(&sb, "  ...(%d more %s)\n", len(list)-12, label)
				break
			}
			pend := make([]string, 0, len(t.pendingEviction))
			for _, p := range t.pendingEviction {
				pend = append(pend, fmt.Sprintf("%s", p))
			}
			fmt.Fprintf(&sb, "  %s[%d] addr=%x action=%v fromLocal=%v awaitingInvRsp=%v\n",
				label, i, t.evictingAddr, t.action, t.fromLocal, pend)
		}
	}
	dumpOut("invToOutOwn", bs.inflightInvToOutsideOwn)
	dumpOut("invToOutRemote", bs.inflightInvToOutsideRemote)

	for i, it := range bs.inflightInvToBottom {
		if i >= 12 {
			fmt.Fprintf(&sb, "  ...(%d more invToBottom)\n", nBot-12)
			break
		}
		var addr uint64
		if it.req != nil {
			addr = it.req.Address
		}
		fmt.Fprintf(&sb, "  invToBottom[%d] addr=%x ack=%d numInv=%d\n", i, addr, it.ack, it.numInv)
	}

	fmt.Print(sb.String()) // STDOUT (captured by the run's out.txt)
}
