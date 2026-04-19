// Package vm provides the models for address translations
package vm

import (
	"github.com/sarchlab/akita/v4/sim"
)

// A TranslationReq asks the receiver component to translate the request.
type TranslationReq struct {
	sim.MsgMeta

	VAddr           uint64
	PID             PID
	AccessCount     uint8
	DeviceID        uint64
	ForWrite        bool
	ForMigration    bool
	ForInvalidation bool
}

// Meta returns the meta data associated with the message.
func (r *TranslationReq) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns cloned TranslationReq with different ID
func (r *TranslationReq) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// TranslationReqBuilder can build translation requests
type TranslationReqBuilder struct {
	src, dst        sim.RemotePort
	vAddr           uint64
	pid             PID
	AccessCount     uint8
	deviceID        uint64
	forWrite        bool
	forMigration    bool
	forInvalidation bool
}

// WithSrc sets the source of the request to build.
func (b TranslationReqBuilder) WithSrc(
	src sim.RemotePort,
) TranslationReqBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the request to build.
func (b TranslationReqBuilder) WithDst(
	dst sim.RemotePort,
) TranslationReqBuilder {
	b.dst = dst
	return b
}

// WithVAddr sets the virtual address of the request to build.
func (b TranslationReqBuilder) WithVAddr(vAddr uint64) TranslationReqBuilder {
	b.vAddr = vAddr
	return b
}

// WithPID sets the virtual address of the request to build.
func (b TranslationReqBuilder) WithPID(pid PID) TranslationReqBuilder {
	b.pid = pid
	return b
}

func (b TranslationReqBuilder) WithAccessCount(ac uint8) TranslationReqBuilder {
	b.AccessCount = ac
	return b
}

// WithDeviceID sets the GPU ID of the request to build.
func (b TranslationReqBuilder) WithDeviceID(
	deviceID uint64,
) TranslationReqBuilder {
	b.deviceID = deviceID
	return b
}

func (b TranslationReqBuilder) SetForWrite(bo bool) TranslationReqBuilder {
	b.forWrite = bo
	return b
}

func (b TranslationReqBuilder) SetForMigration(bo bool) TranslationReqBuilder {
	b.forMigration = bo
	return b
}

func (b TranslationReqBuilder) SetForInvalidation(bo bool) TranslationReqBuilder {
	b.forInvalidation = bo
	return b
}

// Build creates a new TranslationReq
func (b TranslationReqBuilder) Build() *TranslationReq {
	r := &TranslationReq{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.VAddr = b.vAddr
	r.PID = b.pid
	r.DeviceID = b.deviceID
	r.ForMigration = b.forMigration
	r.ForInvalidation = b.forInvalidation

	return r
}

// A TranslationRsp is the respond for a TranslationReq. It carries the physical
// address.
type TranslationRsp struct {
	sim.MsgMeta

	RespondTo         string // The ID of the request it replies
	Page              Page
	IsMigrationRsp    bool
	IsInvalidationRsp bool
	OriginalReq       TranslationReq
}

// Meta returns the meta data associated with the message.
func (r *TranslationRsp) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns cloned TranslationRsp with different ID
func (r *TranslationRsp) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// GetRspTo returns the request ID that the respond is responding to.
func (r *TranslationRsp) GetRspTo() string {
	return r.RespondTo
}

// TranslationRspBuilder can build translation requests
type TranslationRspBuilder struct {
	src, dst          sim.RemotePort
	rspTo             string
	page              Page
	IsMigrationRsp    bool
	IsInvalidationRsp bool
	OriginalReq       TranslationReq
}

// WithSrc sets the source of the respond to build.
func (b TranslationRspBuilder) WithSrc(
	src sim.RemotePort,
) TranslationRspBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the respond to build.
func (b TranslationRspBuilder) WithDst(
	dst sim.RemotePort,
) TranslationRspBuilder {
	b.dst = dst
	return b
}

// WithRspTo sets the request ID of the respond to build.
func (b TranslationRspBuilder) WithRspTo(rspTo string) TranslationRspBuilder {
	b.rspTo = rspTo
	return b
}

// WithPage sets the page of the respond to build.
func (b TranslationRspBuilder) WithPage(page Page) TranslationRspBuilder {
	b.page = page
	return b
}

// WithPage sets the page of the respond to build.
func (b TranslationRspBuilder) SetIsMigrationRsp(bo bool) TranslationRspBuilder {
	b.IsMigrationRsp = bo
	return b
}

// WithPage sets the page of the respond to build.
func (b TranslationRspBuilder) SetIsInvalidationRsp(bo bool) TranslationRspBuilder {
	b.IsInvalidationRsp = bo
	return b
}

func (b TranslationRspBuilder) WithOriginalReq(originalReq TranslationReq) TranslationRspBuilder {
	b.OriginalReq = originalReq
	return b
}

// Build creates a new TranslationRsp
func (b TranslationRspBuilder) Build() *TranslationRsp {
	r := &TranslationRsp{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.RespondTo = b.rspTo
	r.Page = b.page
	r.IsMigrationRsp = b.IsMigrationRsp
	r.IsInvalidationRsp = b.IsInvalidationRsp
	r.OriginalReq = b.OriginalReq

	return r
}

// PageMigrationInfo records the information required for the driver to perform
// a page migration.
type PageMigrationInfo struct {
	GPUReqToVAddrMap map[uint64][]uint64
}

// PageMigrationReqToDriver is a req to driver from MMU to start page migration
// process
type PageMigrationReqToDriver struct {
	sim.MsgMeta

	StartTime         sim.VTimeInSec
	EndTime           sim.VTimeInSec
	MigrationInfo     *PageMigrationInfo
	CurrAccessingGPUs []uint64
	PID               PID
	CurrPageHostGPU   uint64
	PageSize          uint64
	RespondToTop      bool
	RequestingDevice  uint64
	ForDuplication    bool
	ForInvalidation   bool
}

// Meta returns the meta data associated with the message.
func (m *PageMigrationReqToDriver) Meta() *sim.MsgMeta {
	return &m.MsgMeta
}

// Clone returns cloned PageMigrationReqToDriver with different ID
func (m *PageMigrationReqToDriver) Clone() sim.Msg {
	return m
}

func (m *PageMigrationReqToDriver) GenerateRsp() sim.Rsp {
	rsp := NewPageMigrationRspFromDriver(m.Dst, m.Src, m)

	return rsp
}

// NewPageMigrationReqToDriver creates a PageMigrationReqToDriver.
func NewPageMigrationReqToDriver(
	src, dst sim.RemotePort,
) *PageMigrationReqToDriver {
	cmd := new(PageMigrationReqToDriver)
	cmd.Src = src
	cmd.Dst = dst
	cmd.ID = sim.GetIDGenerator().Generate()

	return cmd
}

// PageMigrationRspFromDriver is a rsp from driver to MMU marking completion of
// migration
type PageMigrationRspFromDriver struct {
	sim.MsgMeta

	StartTime sim.VTimeInSec
	EndTime   sim.VTimeInSec
	VAddr     []uint64
	RspToTop  bool

	OriginalReq sim.Msg
}

// Meta returns the meta data associated with the message.
func (m *PageMigrationRspFromDriver) Meta() *sim.MsgMeta {
	return &m.MsgMeta
}

// Clone returns cloned PageMigrationRspFromDriver with different ID
func (m *PageMigrationRspFromDriver) Clone() sim.Msg {
	return m
}

func (m *PageMigrationRspFromDriver) GetRspTo() string {
	return m.OriginalReq.Meta().ID
}

// NewPageMigrationRspFromDriver creates a new PageMigrationRspFromDriver.
func NewPageMigrationRspFromDriver(
	src, dst sim.RemotePort,
	originalReq sim.Msg,
) *PageMigrationRspFromDriver {
	cmd := new(PageMigrationRspFromDriver)
	cmd.Src = src
	cmd.Dst = dst
	cmd.OriginalReq = originalReq

	return cmd
}

type RemoveRequestInMMUFromDriver struct {
	sim.MsgMeta
}

func (m *RemoveRequestInMMUFromDriver) Meta() *sim.MsgMeta {
	return &m.MsgMeta
}

func (m *RemoveRequestInMMUFromDriver) Clone() sim.Msg {
	return m
}

func NewRemoveRequestInMMUFromDriver(
	src, dst sim.RemotePort,
) *RemoveRequestInMMUFromDriver {
	cmd := new(RemoveRequestInMMUFromDriver)
	cmd.Src = src
	cmd.Dst = dst

	return cmd
}

type RemoveRequestRspFromMMU struct {
	sim.MsgMeta
}

func (m *RemoveRequestRspFromMMU) Meta() *sim.MsgMeta {
	return &m.MsgMeta
}

func (m *RemoveRequestRspFromMMU) Clone() sim.Msg {
	return m
}

func NewRemoveRequestRspFromMMU(
	src, dst sim.RemotePort,
) *RemoveRequestRspFromMMU {
	cmd := new(RemoveRequestRspFromMMU)
	cmd.Src = src
	cmd.Dst = dst

	return cmd
}

// ==========================================================================================
// A TranslationReq asks the receiver component to translate the request.
type PTEInvalidationReq struct {
	sim.MsgMeta

	VAddr    uint64
	PID      PID
	PageSize uint64
}

func (r *PTEInvalidationReq) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

func (r *PTEInvalidationReq) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

type PTEInvalidationReqBuilder struct {
	src, dst sim.RemotePort
	VAddr    uint64
	PID      PID
	PageSize uint64
}

func (b PTEInvalidationReqBuilder) WithSrc(
	src sim.RemotePort,
) PTEInvalidationReqBuilder {
	b.src = src
	return b
}

func (b PTEInvalidationReqBuilder) WithDst(
	dst sim.RemotePort,
) PTEInvalidationReqBuilder {
	b.dst = dst
	return b
}

func (b PTEInvalidationReqBuilder) WithVAddr(
	VAddr uint64,
) PTEInvalidationReqBuilder {
	b.VAddr = VAddr
	return b
}

func (b PTEInvalidationReqBuilder) WithPID(
	PID PID,
) PTEInvalidationReqBuilder {
	b.PID = PID
	return b
}

func (b PTEInvalidationReqBuilder) WithPageSize(
	PageSize uint64,
) PTEInvalidationReqBuilder {
	b.PageSize = PageSize
	return b
}

func (b PTEInvalidationReqBuilder) Build() *PTEInvalidationReq {
	r := &PTEInvalidationReq{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.VAddr = b.VAddr
	r.PID = b.PID
	r.PageSize = b.PageSize

	return r
}

type PTEInvalidationRsp struct {
	sim.MsgMeta
}

func (r *PTEInvalidationRsp) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

func (r *PTEInvalidationRsp) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

type PTEInvalidationRspBuilder struct {
	src, dst sim.RemotePort
}

func (b PTEInvalidationRspBuilder) WithSrc(
	src sim.RemotePort,
) PTEInvalidationRspBuilder {
	b.src = src
	return b
}

func (b PTEInvalidationRspBuilder) WithDst(
	dst sim.RemotePort,
) PTEInvalidationRspBuilder {
	b.dst = dst
	return b
}

func (b PTEInvalidationRspBuilder) Build() *PTEInvalidationRsp {
	r := &PTEInvalidationRsp{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst

	return r
}

// ==========================================================================================

type FlushReq struct {
	sim.MsgMeta
}

// Meta returns the meta data associated with the message.
func (r *FlushReq) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns cloned FlushReq with different ID
func (r *FlushReq) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// FlushReqBuilder can build AT flush requests
type FlushReqBuilder struct {
	src, dst sim.RemotePort
}

// WithSrc sets the source of the request to build.
func (b FlushReqBuilder) WithSrc(src sim.RemotePort) FlushReqBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the request to build.
func (b FlushReqBuilder) WithDst(dst sim.RemotePort) FlushReqBuilder {
	b.dst = dst
	return b
}

// Build creates a new TLBFlushReq
func (b FlushReqBuilder) Build() *FlushReq {
	r := &FlushReq{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst

	return r
}

type FlushRsp struct {
	sim.MsgMeta
}

// Meta returns the meta data associated with the message.
func (r *FlushRsp) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns cloned FlushReq with different ID
func (r *FlushRsp) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// FlushReqBuilder can build AT flush requests
type FlushRspBuilder struct {
	src, dst sim.RemotePort
}

// WithSrc sets the source of the request to build.
func (b FlushRspBuilder) WithSrc(src sim.RemotePort) FlushRspBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the request to build.
func (b FlushRspBuilder) WithDst(dst sim.RemotePort) FlushRspBuilder {
	b.dst = dst
	return b
}

// Build creates a new TLBFlushReq
func (b FlushRspBuilder) Build() *FlushRsp {
	r := &FlushRsp{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst

	return r
}

// ==========================================================================================

type RestartReq struct {
	sim.MsgMeta
}

// Meta returns the meta data associated with the message.
func (r *RestartReq) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns cloned FlushReq with different ID
func (r *RestartReq) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// FlushReqBuilder can build AT flush requests
type RestartReqBuilder struct {
	src, dst sim.RemotePort
}

// WithSrc sets the source of the request to build.
func (b RestartReqBuilder) WithSrc(src sim.RemotePort) RestartReqBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the request to build.
func (b RestartReqBuilder) WithDst(dst sim.RemotePort) RestartReqBuilder {
	b.dst = dst
	return b
}

// Build creates a new TLBFlushReq
func (b RestartReqBuilder) Build() *RestartReq {
	r := &RestartReq{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst

	return r
}

type RestartRsp struct {
	sim.MsgMeta
}

// Meta returns the meta data associated with the message.
func (r *RestartRsp) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns cloned RestartReq with different ID
func (r *RestartRsp) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// RestartReqBuilder can build AT Restart requests
type RestartRspBuilder struct {
	src, dst sim.RemotePort
}

// WithSrc sets the source of the request to build.
func (b RestartRspBuilder) WithSrc(src sim.RemotePort) RestartRspBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the request to build.
func (b RestartRspBuilder) WithDst(dst sim.RemotePort) RestartRspBuilder {
	b.dst = dst
	return b
}

// Build creates a new TLBRestartReq
func (b RestartRspBuilder) Build() *RestartRsp {
	r := &RestartRsp{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst

	return r
}
