package globalmshr

import (
	"github.com/sarchlab/akita/v4/mem/cache/globalMSHR/internal"
	"github.com/sarchlab/akita/v4/mem/mem"

	"github.com/sarchlab/akita/v4/sim"
)

type Comp struct {
	*sim.TickingComponent
	sim.MiddlewareHolder

	name     string
	deviceID int

	topPort     sim.Port
	bottomPort  sim.Port
	controlPort sim.Port

	addressToPortMapper mem.AddressToPortMapper
	log2UnitSize        uint64 // byte 단위
	log2BlockSize       uint64 // cacheline 크기

	mshr internal.MSHR

	sendToTop []sim.Msg
}

func (c *Comp) SetAddressToPortMapper(lmf mem.AddressToPortMapper) {
	c.addressToPortMapper = lmf
}

func (c *Comp) Tick() bool {
	madeProgress := false
	temp := false

	temp = c.SendToTop()
	madeProgress = temp || madeProgress

	temp = c.SendRequest()
	madeProgress = temp || madeProgress

	return madeProgress
}

func (c *Comp) SendToTop() bool {
	if len(c.sendToTop) == 0 {
		return false
	}

	if !c.topPort.CanSend() {
		return false
	}

	err := c.topPort.Send(c.sendToTop[0])
	if err != nil {
		return false
	}

	c.sendToTop = c.sendToTop[1:]
	return true
}

func (c *Comp) SendRequest() bool {
	msg := c.topPort.PeekIncoming()
	if msg == nil {
		return false
	}

	switch msg := msg.(type) {
	case *mem.ReadReq:
		// mshr entry 확인 후 병합
		return c.SendReadReqToBottom(msg)
	case *mem.WriteReq:
		// mshr entry 확인 후 request 추가, 아래로 전송
		return c.SendWriteReqToBottom(msg)
	}

	return false
}

func (c *Comp) ReceiveResponse() bool {
	msg := c.bottomPort.PeekIncoming()
	if msg == nil {
		return false
	}

	switch msg := msg.(type) {
	case *mem.DataReadyRsp:
		// mshr entry 확인 후,
		return c.ProcessDataReadyRsp(msg)
	case *mem.WriteDoneRsp:
		// mshr entry 확인 후, 응답 전송
		return c.ProcessWriteDoneRsp(msg)
	}

	return false
}

func (c *Comp) SendReadReqToBottom(msg *mem.ReadReq) bool {
	pid := msg.PID
	addr := msg.Address >> c.log2UnitSize << c.log2UnitSize
	sendToBottom := false

	entry := c.mshr.Query(pid, addr)
	if entry == nil {
		sendToBottom = true
	} else if entry.Fetching == 0 {
		sendToBottom = true
	}

	if sendToBottom {
		if !c.bottomPort.CanSend() {
			return false
		}

		newMsg := *msg
		newMsg.Src = c.bottomPort.AsRemote()
		newMsg.Dst = c.addressToPortMapper.Find(addr)
		newMsg.AccessByteSize = 1 << c.log2UnitSize
		newMsg.Address = addr
		// remote L2 cache에서 cacheline 단위로 response 보내줄 예정?

		entry = c.mshr.Add(msg)
		entry.RequestToBottom = append(entry.RequestToBottom, &newMsg)
		entry.Fetching = 1 << (c.log2UnitSize - c.log2BlockSize)

		c.bottomPort.Send(&newMsg)

		return true
	}

	c.mshr.Add(msg)

	c.topPort.RetrieveIncoming()
	return true
}

func (c *Comp) ProcessDataReadyRsp(msg *mem.DataReadyRsp) bool {
	rsp := msg.Clone().(*mem.DataReadyRsp)
	addr := msg.Origin.GetAddress()
	index := c.addrToIndex(addr)

	i, k := c.findEntryByRequestID(msg.RespondTo)
	if i == -1 {
		// 그냥 해당하는 cache에 전송해버리기
		rsp.RespondTo = ""
		rsp.Dst = c.addressToPortMapper.Find(addr)
		c.sendToTop = append(c.sendToTop, rsp)

		c.bottomPort.RetrieveIncoming()
		return true
	}

	entry := c.mshr.AllEntries()[i]
	j := c.findRequestByIndex(index, entry)
	reqFromTop := entry.RequestFromTop[j].Req

	if j == -1 {
		// 그냥 해당하는 cache에 전송해버리기
		rsp.RespondTo = ""
		rsp.Dst = c.addressToPortMapper.Find(addr)
	} else {
		rsp.RespondTo = reqFromTop.Meta().ID
		rsp.Dst = reqFromTop.Meta().Src
	}

	c.sendToTop = append(c.sendToTop, rsp)

	entry.Fetching--
	if entry.Fetching == 0 {
		entry.RequestToBottom = append(entry.RequestToBottom[:k], entry.RequestToBottom[k+1:]...)
	}
	if len(entry.RequestToBottom) == 0 {
		c.mshr.RemoveEntryByIndex(j)
	}

	c.bottomPort.RetrieveIncoming()
	return true
}

func (c *Comp) SendWriteReqToBottom(msg *mem.WriteReq) bool {
	if !c.bottomPort.CanSend() {
		return false
	}

	entry := c.mshr.Add(msg)

	newMsg := *msg
	newMsg.Src = c.bottomPort.AsRemote()
	newMsg.Dst = c.addressToPortMapper.Find(msg.Address)
	entry.RequestToBottom = append(entry.RequestToBottom, &newMsg)

	c.bottomPort.Send(&newMsg)

	c.topPort.RetrieveIncoming()
	return true
}

func (c *Comp) ProcessWriteDoneRsp(msg *mem.WriteDoneRsp) bool {
	rsp := msg.Clone().(*mem.WriteDoneRsp)
	addr := msg.Origin.GetAddress()
	index := c.addrToIndex(addr)

	i, _ := c.findEntryByRequestID(msg.RespondTo)
	if i == -1 {
		// 버리기
		c.bottomPort.RetrieveIncoming()
		return false
	}

	entry := c.mshr.AllEntries()[i]
	j := c.findRequestByIndex(index, entry)
	reqFromTop := entry.RequestFromTop[j].Req

	if j == -1 {
		// 버리기
		c.bottomPort.RetrieveIncoming()
		return false
	}

	rsp.RespondTo = reqFromTop.Meta().ID
	rsp.Dst = reqFromTop.Meta().Src

	c.sendToTop = append(c.sendToTop, rsp)

	if entry.Fetching == 0 {
		c.mshr.RemoveEntryByIndex(i)
	}

	return true
}

func (c *Comp) findEntryByRequestID(id string) (int, int) {
	for i, e := range c.mshr.AllEntries() {
		for j, req := range e.RequestToBottom {
			if req.Meta().ID == id {
				return i, j
			}
		}
	}

	return -1, -1
}

func (c *Comp) findRequestByIndex(index uint64, entry *internal.MSHREntry) int {
	for i, req := range entry.RequestFromTop {
		if req.Index == index {
			return i
		}
	}

	return -1
}

func (c *Comp) addrToIndex(addr uint64) uint64 {
	return addr % (1 << c.log2UnitSize) / (1 << c.log2BlockSize)
}
