package mem

import (
	"reflect"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

var accessReqByteOverhead = 12
var accessRspByteOverhead = 4
var controlMsgByteOverhead = 4

// AccessReq abstracts read and write requests that are sent to the
// cache modules or memory controllers.
type AccessReq interface {
	sim.Msg
	GetAddress() uint64
	SetAddress(addr uint64)
	GetVAddr() uint64
	GetByteSize() uint64
	GetPID() vm.PID

	GetReqFrom() string
	SetReqFrom(string)

	GetSrcRDMA() sim.RemotePort
	SetSrcRDMA(port sim.RemotePort)

	GetNoNeedToReply() bool
	SetNoNeedToReply(b bool)

	GetNiceness() bool
	SetNiceness(b bool)
}

// A AccessRsp is a respond in the memory system.
type AccessRsp interface {
	sim.Msg
	sim.Rsp
	GetOrigin() AccessReq
	GetWaitFor() uint64
}

// A ReadReq is a request sent to a memory controller to fetch data
type ReadReq struct {
	sim.MsgMeta

	Address            uint64
	VAddr              uint64
	AccessByteSize     uint64
	PID                vm.PID
	CanWaitForCoalesce bool
	Info               interface{}

	ReqFrom string
	SrcRDMA sim.RemotePort

	NoNeedToReply     bool // 응답이 필요없는 request인 경우 (prefetch?)
	Niceness          bool // 우선순위가 낮아져도 되는 request
	FetchForWriteMiss bool
}

// Meta returns the message meta.
func (r *ReadReq) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns cloned ReadReq with different ID
func (r *ReadReq) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// GenerateRsp generate DataReadyRsp to ReadReq
func (r *ReadReq) GenerateRsp(data []byte) sim.Rsp {
	rsp := DataReadyRspBuilder{}.
		WithSrc(r.Dst).
		WithDst(r.Src).
		WithRspTo(r.ID).
		WithData(data).
		Build()

	return rsp
}

// GetByteSize returns the number of byte that the request is accessing.
func (r *ReadReq) GetByteSize() uint64 {
	return r.AccessByteSize
}

// GetAddress returns the address that the request is accessing
func (r *ReadReq) GetAddress() uint64 {
	return r.Address
}

func (r *ReadReq) SetAddress(addr uint64) {
	r.Address = addr
}

// GetAddress returns the address that the request is accessing
func (r *ReadReq) GetVAddr() uint64 {
	return r.VAddr
}

// GetPID returns the process ID that the request is working on.
func (r *ReadReq) GetPID() vm.PID {
	return r.PID
}

func (r *ReadReq) GetReqFrom() string {
	return r.ReqFrom
}

func (r *ReadReq) SetReqFrom(reqFrom string) {
	r.ReqFrom = reqFrom
}

func (r *ReadReq) GetSrcRDMA() sim.RemotePort {
	return r.SrcRDMA
}

func (r *ReadReq) SetSrcRDMA(port sim.RemotePort) {
	r.SrcRDMA = port
}

func (r *ReadReq) GetNoNeedToReply() bool {
	return r.NoNeedToReply
}

func (r *ReadReq) SetNoNeedToReply(b bool) {
	r.NoNeedToReply = b
}

func (r *ReadReq) GetNiceness() bool {
	return r.Niceness
}

func (r *ReadReq) SetNiceness(b bool) {
	r.Niceness = b
}

// ReadReqBuilder can build read requests.
type ReadReqBuilder struct {
	src, dst           sim.RemotePort
	pid                vm.PID
	address, byteSize  uint64
	VAddr              uint64
	canWaitForCoalesce bool
	info               interface{}
	ReqFrom            string
	SrcRDMA            sim.RemotePort
	FetchForWriteMiss  bool
}

// WithSrc sets the source of the request to build.
func (b ReadReqBuilder) WithSrc(src sim.RemotePort) ReadReqBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the request to build.
func (b ReadReqBuilder) WithDst(dst sim.RemotePort) ReadReqBuilder {
	b.dst = dst
	return b
}

// WithPID sets the PID of the request to build.
func (b ReadReqBuilder) WithPID(pid vm.PID) ReadReqBuilder {
	b.pid = pid
	return b
}

// WithInfo sets the Info of the request to build.
func (b ReadReqBuilder) WithInfo(info interface{}) ReadReqBuilder {
	b.info = info
	return b
}

// WithAddress sets the address of the request to build.
func (b ReadReqBuilder) WithAddress(address uint64) ReadReqBuilder {
	b.address = address
	return b
}

// WithAddress sets the address of the request to build.
func (b ReadReqBuilder) WithVAddr(address uint64) ReadReqBuilder {
	b.VAddr = address
	return b
}

// WithByteSize sets the byte size of the request to build.
func (b ReadReqBuilder) WithByteSize(byteSize uint64) ReadReqBuilder {
	b.byteSize = byteSize
	return b
}

// CanWaitForCoalesce allow the request to build to wait for coalesce.
func (b ReadReqBuilder) CanWaitForCoalesce() ReadReqBuilder {
	b.canWaitForCoalesce = true
	return b
}

// CanWaitForCoalesce allow the request to build to wait for coalesce.
func (b ReadReqBuilder) WithReqFrom(ReqFrom string) ReadReqBuilder {
	b.ReqFrom = ReqFrom
	return b
}

func (b ReadReqBuilder) WithSrcRDMA(port sim.RemotePort) ReadReqBuilder {
	b.SrcRDMA = port
	return b
}

func (b ReadReqBuilder) WithFetchForWriteMiss(bo bool) ReadReqBuilder {
	b.FetchForWriteMiss = bo
	return b
}

// Build creates a new ReadReq
func (b ReadReqBuilder) Build() *ReadReq {
	r := &ReadReq{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.TrafficBytes = accessReqByteOverhead
	r.Address = b.address
	r.VAddr = b.VAddr
	r.PID = b.pid
	r.Info = b.info
	r.AccessByteSize = b.byteSize
	r.CanWaitForCoalesce = b.canWaitForCoalesce
	r.TrafficClass = reflect.TypeOf(ReadReq{}).String()
	r.ReqFrom = b.ReqFrom
	r.SrcRDMA = b.SrcRDMA
	r.FetchForWriteMiss = b.FetchForWriteMiss

	return r
}

// A WriteReq is a request sent to a memory controller to write data
type WriteReq struct {
	sim.MsgMeta

	Address            uint64
	VAddr              uint64
	Data               []byte
	DirtyMask          []bool
	PID                vm.PID
	CanWaitForCoalesce bool
	Info               interface{}

	ReqFrom string
	SrcRDMA sim.RemotePort

	NoNeedToReply bool
	Niceness      bool
}

// Meta returns the meta data attached to a request.
func (r *WriteReq) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns cloned WriteReq with different ID
func (r *WriteReq) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// GenerateRsp generate WriteDoneRsp to the original WriteReq
func (r *WriteReq) GenerateRsp() sim.Rsp {
	rsp := WriteDoneRspBuilder{}.
		WithSrc(r.Dst).
		WithDst(r.Src).
		WithRspTo(r.ID).
		Build()

	return rsp
}

// GetByteSize returns the number of byte that the request is writing.
func (r *WriteReq) GetByteSize() uint64 {
	return uint64(len(r.Data))
}

// GetAddress returns the address that the request is accessing
func (r *WriteReq) GetAddress() uint64 {
	return r.Address
}

func (r *WriteReq) SetAddress(addr uint64) {
	r.Address = addr
}

// GetAddress returns the address that the request is accessing
func (r *WriteReq) GetVAddr() uint64 {
	return r.VAddr
}

// GetPID returns the PID of the read address
func (r *WriteReq) GetPID() vm.PID {
	return r.PID
}

func (r *WriteReq) GetReqFrom() string {
	return r.ReqFrom
}

func (r *WriteReq) SetReqFrom(reqFrom string) {
	r.ReqFrom = reqFrom
}

func (r *WriteReq) GetSrcRDMA() sim.RemotePort {
	return r.SrcRDMA
}

func (r *WriteReq) SetSrcRDMA(port sim.RemotePort) {
	r.SrcRDMA = port
}

func (r *WriteReq) GetNoNeedToReply() bool {
	return r.NoNeedToReply
}

func (r *WriteReq) SetNoNeedToReply(b bool) {
	r.NoNeedToReply = b
}

func (r *WriteReq) GetNiceness() bool {
	return r.Niceness
}

func (r *WriteReq) SetNiceness(b bool) {
	r.Niceness = b
}

// WriteReqBuilder can build read requests.
type WriteReqBuilder struct {
	src, dst           sim.RemotePort
	pid                vm.PID
	info               interface{}
	address            uint64
	VAddr              uint64
	data               []byte
	dirtyMask          []bool
	canWaitForCoalesce bool
	ReqFrom            string
	SrcRDMA            sim.RemotePort
}

// WithSrc sets the source of the request to build.
func (b WriteReqBuilder) WithSrc(src sim.RemotePort) WriteReqBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the request to build.
func (b WriteReqBuilder) WithDst(dst sim.RemotePort) WriteReqBuilder {
	b.dst = dst
	return b
}

// WithPID sets the PID of the request to build.
func (b WriteReqBuilder) WithPID(pid vm.PID) WriteReqBuilder {
	b.pid = pid
	return b
}

// WithInfo sets the information attached to the request to build.
func (b WriteReqBuilder) WithInfo(info interface{}) WriteReqBuilder {
	b.info = info
	return b
}

// WithAddress sets the address of the request to build.
func (b WriteReqBuilder) WithAddress(address uint64) WriteReqBuilder {
	b.address = address
	return b
}

// WithAddress sets the address of the request to build.
func (b WriteReqBuilder) WithVAddr(address uint64) WriteReqBuilder {
	b.VAddr = address
	return b
}

// WithData sets the data of the request to build.
func (b WriteReqBuilder) WithData(data []byte) WriteReqBuilder {
	b.data = data
	return b
}

// WithDirtyMask sets the dirty mask of the request to build.
func (b WriteReqBuilder) WithDirtyMask(mask []bool) WriteReqBuilder {
	b.dirtyMask = mask
	return b
}

// CanWaitForCoalesce allow the request to build to wait for coalesce.
func (b WriteReqBuilder) CanWaitForCoalesce() WriteReqBuilder {
	b.canWaitForCoalesce = true
	return b
}

// CanWaitForCoalesce allow the request to build to wait for coalesce.
func (b WriteReqBuilder) WithReqFrom(ReqFrom string) WriteReqBuilder {
	b.ReqFrom = ReqFrom
	return b
}

func (b WriteReqBuilder) WithSrcRDMA(port sim.RemotePort) WriteReqBuilder {
	b.SrcRDMA = port
	return b
}

// Build creates a new WriteReq
func (b WriteReqBuilder) Build() *WriteReq {
	r := &WriteReq{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.PID = b.pid
	r.Info = b.info
	r.Address = b.address
	r.VAddr = b.VAddr
	r.Data = b.data
	r.TrafficBytes = len(r.Data) + accessReqByteOverhead
	r.DirtyMask = b.dirtyMask
	r.CanWaitForCoalesce = b.canWaitForCoalesce
	r.TrafficClass = reflect.TypeOf(WriteReq{}).String()
	r.ReqFrom = b.ReqFrom
	r.SrcRDMA = b.SrcRDMA

	return r
}

// A DataReadyRsp is the respond sent from the lower module to the higher
// module that carries the data loaded.
type DataReadyRsp struct {
	sim.MsgMeta

	RespondTo string // The ID of the request it replies
	Data      []byte
	Origin    AccessReq
	WaitFor   uint64
}

// Meta returns the meta data attached to each message.
func (r *DataReadyRsp) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns cloned DataReadyRsp with different ID
func (r *DataReadyRsp) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// GetRspTo returns the ID if the request that the respond is responding to.
func (r *DataReadyRsp) GetRspTo() string {
	return r.RespondTo
}

func (r *DataReadyRsp) GetOrigin() AccessReq {
	return r.Origin
}

func (r *DataReadyRsp) GetWaitFor() uint64 {
	return r.WaitFor
}

// DataReadyRspBuilder can build data ready responds.
type DataReadyRspBuilder struct {
	src, dst sim.RemotePort
	rspTo    string
	data     []byte
	origin   AccessReq
	waitFor  uint64
}

// WithSrc sets the source of the request to build.
func (b DataReadyRspBuilder) WithSrc(src sim.RemotePort) DataReadyRspBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the request to build.
func (b DataReadyRspBuilder) WithDst(dst sim.RemotePort) DataReadyRspBuilder {
	b.dst = dst
	return b
}

// WithRspTo sets ID of the request that the respond to build is replying to.
func (b DataReadyRspBuilder) WithRspTo(id string) DataReadyRspBuilder {
	b.rspTo = id
	return b
}

// WithData sets the data of the request to build.
func (b DataReadyRspBuilder) WithData(data []byte) DataReadyRspBuilder {
	b.data = data
	return b
}

func (b DataReadyRspBuilder) WithOrigin(origin AccessReq) DataReadyRspBuilder {
	b.origin = origin
	return b
}

// Build creates a new DataReadyRsp
func (b DataReadyRspBuilder) Build() *DataReadyRsp {
	r := &DataReadyRsp{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.TrafficBytes = len(b.data) + accessRspByteOverhead
	r.RespondTo = b.rspTo
	r.Data = b.data
	r.TrafficClass = reflect.TypeOf(ReadReq{}).String()
	r.Origin = b.origin
	r.WaitFor = b.waitFor

	return r
}

// A WriteDoneRsp is a respond sent from the lower module to the higher module
// to mark a previous requests is completed successfully.
type WriteDoneRsp struct {
	sim.MsgMeta

	RespondTo string
	Origin    AccessReq
	WaitFor   uint64
}

// Meta returns the meta data associated with the message.
func (r *WriteDoneRsp) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns cloned WriteDoneRsp with different ID
func (r *WriteDoneRsp) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// GetRspTo returns the ID of the request that the respond is responding to.
func (r *WriteDoneRsp) GetRspTo() string {
	return r.RespondTo
}

func (r *WriteDoneRsp) GetOrigin() AccessReq {
	return r.Origin
}

func (r *WriteDoneRsp) GetWaitFor() uint64 {
	return r.WaitFor
}

// WriteDoneRspBuilder can build data ready responds.
type WriteDoneRspBuilder struct {
	src, dst sim.RemotePort
	rspTo    string
	addr     uint64
	origin   AccessReq
	waitFor  uint64
}

// WithSrc sets the source of the request to build.
func (b WriteDoneRspBuilder) WithSrc(src sim.RemotePort) WriteDoneRspBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the request to build.
func (b WriteDoneRspBuilder) WithDst(dst sim.RemotePort) WriteDoneRspBuilder {
	b.dst = dst
	return b
}

// WithRspTo sets ID of the request that the respond to build is replying to.
func (b WriteDoneRspBuilder) WithRspTo(id string) WriteDoneRspBuilder {
	b.rspTo = id
	return b
}

func (b WriteDoneRspBuilder) WithOrigin(origin AccessReq) WriteDoneRspBuilder {
	b.origin = origin
	return b
}

// Build creates a new WriteDoneRsp
func (b WriteDoneRspBuilder) Build() *WriteDoneRsp {
	r := &WriteDoneRsp{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.TrafficBytes = accessRspByteOverhead
	r.RespondTo = b.rspTo
	r.TrafficClass = reflect.TypeOf(WriteReq{}).String()
	r.Origin = b.origin
	r.WaitFor = b.waitFor

	return r
}

// ControlMsg is the commonly used message type for controlling the components
// on the memory hierarchy. It is also used for responding the original
// requester with the Done field. Drain is used to process all the requests in
// the queue when drain happens the component will not accept any new requests
// Enable enables the component work. If the enable = false, it will not process
// any requests.
type ControlMsg struct {
	sim.MsgMeta

	DiscardTransations bool
	Restart            bool
	NotifyDone         bool
	Enable             bool
	Drain              bool
	Flush              bool
	Pause              bool
	Invalid            bool
}

// Meta returns the meta data associated with the ControlMsg.
func (m *ControlMsg) Meta() *sim.MsgMeta {
	return &m.MsgMeta
}

// Clone returns cloned ControlMsg with different ID
func (m *ControlMsg) Clone() sim.Msg {
	cloneMsg := *m
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// GenerateRsp generates a GeneralRsp for ControlMsg.
func (m *ControlMsg) GenerateRsp() sim.Rsp {
	rsp := sim.GeneralRspBuilder{}.
		WithSrc(m.Dst).
		WithDst(m.Src).
		WithOriginalReq(m).
		Build()

	return rsp
}

// A ControlMsgBuilder can build control messages.
type ControlMsgBuilder struct {
	src, dst            sim.RemotePort
	discardTransactions bool
	restart             bool
	notifyDone          bool
	Enable              bool
	Drain               bool
	Flush               bool
	Pause               bool
	Invalid             bool
}

// WithSrc sets the source of the request to build.
func (b ControlMsgBuilder) WithSrc(src sim.RemotePort) ControlMsgBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the request to build.
func (b ControlMsgBuilder) WithDst(dst sim.RemotePort) ControlMsgBuilder {
	b.dst = dst
	return b
}

// ToDiscardTransactions sets the discard transactions bit of the control
// messages to 1.
func (b ControlMsgBuilder) ToDiscardTransactions() ControlMsgBuilder {
	b.discardTransactions = true
	return b
}

// ToRestart sets the restart bit of the control messages to 1.
func (b ControlMsgBuilder) ToRestart() ControlMsgBuilder {
	b.restart = true
	return b
}

// ToNotifyDone sets the "notify done" bit of the control messages to 1.
func (b ControlMsgBuilder) ToNotifyDone() ControlMsgBuilder {
	b.notifyDone = true
	return b
}

// WithCtrlInfo sets the enable bit of the control messages to 1.
func (b ControlMsgBuilder) WithCtrlInfo(
	enable bool, drain bool, flush bool, pause bool, invalid bool,
) ControlMsgBuilder {
	b.Enable = enable
	b.Drain = drain
	b.Flush = flush
	b.Pause = pause
	b.Invalid = invalid

	return b
}

// Build creates a new ControlMsg.
func (b ControlMsgBuilder) Build() *ControlMsg {
	m := &ControlMsg{}
	m.ID = sim.GetIDGenerator().Generate()
	m.Src = b.src
	m.Dst = b.dst
	m.TrafficBytes = controlMsgByteOverhead

	m.Enable = b.Enable
	m.Drain = b.Drain
	m.Flush = b.Flush
	m.Invalid = b.Invalid

	m.DiscardTransations = b.discardTransactions
	m.Restart = b.restart
	m.NotifyDone = b.notifyDone
	m.Enable = b.Enable
	m.Drain = b.Drain
	m.Flush = b.Flush
	m.Pause = b.Pause
	m.Invalid = b.Invalid
	m.TrafficClass = reflect.TypeOf(ControlMsg{}).String()

	return m
}

// A ReadReq is a request sent to a memory controller to fetch data
type InvReq struct {
	sim.MsgMeta

	PID        vm.PID
	Address    uint64
	ReqFrom    string
	DstRDMA    sim.RemotePort
	RegionID   int
	IsWriteInv bool // true if caused by a write (vs. eviction/promotion/demotion)
}

// Meta returns the message meta.
func (r *InvReq) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns cloned InvReq with different ID
func (r *InvReq) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// GetAddress returns the address that the request is accessing
func (r *InvReq) GetAddress() uint64 {
	return r.Address
}

// GetPID returns the process ID that the request is working on.
func (r *InvReq) GetPID() vm.PID {
	return r.PID
}

// InvReqBuilder can build read requests.
type InvReqBuilder struct {
	src, dst   sim.RemotePort
	pid        vm.PID
	address    uint64
	reqFrom    string
	dstRDMA    sim.RemotePort
	RegionID   int
	isWriteInv bool
}

// WithSrc sets the source of the request to build.
func (b InvReqBuilder) WithSrc(src sim.RemotePort) InvReqBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the request to build.
func (b InvReqBuilder) WithDst(dst sim.RemotePort) InvReqBuilder {
	b.dst = dst
	return b
}

// WithPID sets the PID of the request to build.
func (b InvReqBuilder) WithPID(pid vm.PID) InvReqBuilder {
	b.pid = pid
	return b
}

// WithAddress sets the address of the request to build.
func (b InvReqBuilder) WithAddress(address uint64) InvReqBuilder {
	b.address = address
	return b
}

// WithAddress sets the address of the request to build.
func (b InvReqBuilder) WithReqFrom(ID string) InvReqBuilder {
	b.reqFrom = ID
	return b
}

// WithAddress sets the address of the request to build.
func (b InvReqBuilder) WithDstRDMA(port sim.RemotePort) InvReqBuilder {
	b.dstRDMA = port
	return b
}

func (b InvReqBuilder) WithRegionID(len int) InvReqBuilder {
	b.RegionID = len
	return b
}

func (b InvReqBuilder) WithIsWriteInv(v bool) InvReqBuilder {
	b.isWriteInv = v
	return b
}

// Build creates a new InvReq
func (b InvReqBuilder) Build() *InvReq {
	r := &InvReq{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.TrafficBytes = accessReqByteOverhead
	r.Address = b.address
	r.PID = b.pid
	r.TrafficClass = reflect.TypeOf(InvReq{}).String()
	r.ReqFrom = b.reqFrom
	r.DstRDMA = b.dstRDMA
	r.RegionID = b.RegionID
	r.IsWriteInv = b.isWriteInv

	return r
}

// A ReadReq is a request sent to a memory controller to fetch data
type InvRsp struct {
	sim.MsgMeta
	RespondTo string
	SrcRDMA   sim.RemotePort
	Accessed  uint64
	NumInv    uint64
}

// Meta returns the message meta.
func (r *InvRsp) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns cloned InvRsp with different ID
func (r *InvRsp) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

func (r *InvRsp) GetRspTo() string {
	return r.RespondTo
}

// InvRspBuilder can build read requests.
type InvRspBuilder struct {
	src, dst  sim.RemotePort
	respondTo string
	srcRDMA   sim.RemotePort
	accessed  uint64
	numInv    uint64
}

// WithSrc sets the source of the request to build.
func (b InvRspBuilder) WithSrc(src sim.RemotePort) InvRspBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the request to build.
func (b InvRspBuilder) WithDst(dst sim.RemotePort) InvRspBuilder {
	b.dst = dst
	return b
}

// WithAddress sets the address of the request to build.
func (b InvRspBuilder) WithRspTo(ID string) InvRspBuilder {
	b.respondTo = ID
	return b
}

func (b InvRspBuilder) WithSrcRDMA(port sim.RemotePort) InvRspBuilder {
	b.srcRDMA = port
	return b
}

func (b InvRspBuilder) WithAccessed(a uint64) InvRspBuilder {
	b.accessed = a
	return b
}

func (b InvRspBuilder) WithNumInv(a uint64) InvRspBuilder {
	b.numInv = a
	return b
}

// Build creates a new InvRsp
func (b InvRspBuilder) Build() *InvRsp {
	r := &InvRsp{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.TrafficBytes = controlMsgByteOverhead
	r.TrafficClass = reflect.TypeOf(InvRsp{}).String()
	r.RespondTo = b.respondTo
	r.SrcRDMA = b.srcRDMA
	r.Accessed = b.accessed
	r.NumInv = b.numInv

	return r
}
