package REC

import (
	"fmt"
	"os"
	"strings"

	"github.com/sarchlab/akita/v4/sim"
)

const deadlockDumpPath = "/root/deadlock_dump.txt"

func appendDeadlockDump(s string) {
	f, err := os.OpenFile(deadlockDumpPath,
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(s)
}

// DumpDeadlockState writes the REC directory's response-egress state to the
// shared deadlock dump on engine halt (RTM-independent). The bottomSender
// drains responses toward the RDMA via RDMADataRspPort; if that egress backs
// up, the home stops emitting acks and the cross-GPU serve deadlocks.
//   sendToRemoteTopQue   = eviction-acks (WriteDoneRsp, Src without "RDMA")
//                          awaiting egress to the original sender.
//   sendToRDMADataRspQue = peer fetch-responses (data RSP) awaiting egress.
// RDMADataRspPortEgress=FULL ⇒ the directory cannot push responses to the RDMA
// (the confirmed return-path back-up point).
func (c *Comp) DumpDeadlockState() {
	bs := c.bottomSender
	if bs == nil {
		return
	}
	// Dump any dir that is a deadlock participant: a backed-up send queue OR
	// outstanding peer/local inflight requests (a home with a big fromOutside
	// backlog shows up here as remoteInflightRequest/numPeerInflightRequest
	// even when its send queues are momentarily empty).
	if len(bs.sendToRemoteTopQue) == 0 &&
		len(bs.sendToRDMADataRspQue) == 0 &&
		len(bs.sendToBottomQue) == 0 &&
		len(bs.sendToRemoteBottomQue) == 0 &&
		len(bs.remoteInflightRequest) == 0 &&
		len(bs.localInflightRequest) == 0 {
		return
	}

	rspPortEgress := recPortEgress(c.RDMADataRspPort)
	reqIngress := "nil"
	if c.RDMADataReqPort != nil {
		if c.RDMADataReqPort.PeekIncoming() != nil {
			reqIngress = "HAS-IN" // peer requests waiting unprocessed at the dir
		} else {
			reqIngress = "empty"
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "[REC %s] localInfl=%d remoteInfl=%d numPeerInfl=%d | sendBottom=%d sendRemoteBottom=%d sendRemoteTop=%d sendRDMARsp=%d | invOwn=%d invRemote=%d | evictAckEgr=%d lostWriteDone=%d rspPortEgress=%s reqPortIngress=%s\n",
		c.name, len(bs.localInflightRequest), len(bs.remoteInflightRequest), bs.numPeerInflightRequest,
		len(bs.sendToBottomQue), len(bs.sendToRemoteBottomQue), len(bs.sendToRemoteTopQue), len(bs.sendToRDMADataRspQue),
		len(bs.inflightInvToOutsideOwn), len(bs.inflightInvToOutsideRemote),
		bs.evictAckEgressedCount, bs.lostWriteDoneRspCount, rspPortEgress, reqIngress)
	appendDeadlockDump(sb.String())
}

func recPortEgress(p sim.Port) string {
	if p == nil {
		return "nil"
	}
	if p.CanSend() {
		return "ok"
	}
	return "FULL"
}
