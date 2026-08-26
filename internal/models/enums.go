package models

// Role is an RBAC role. Roles mirror the operational stations in the factory
// plus the seller-facing role. They are seeded into the roles table and stored
// as a string on the user for simple, index-friendly permission checks.
type Role string

const (
	RoleOwner      Role = "OWNER"
	RoleAdmin      Role = "ADMIN"
	RoleOps        Role = "OPS"
	RoleDesigner   Role = "DESIGNER"
	RoleProduction Role = "PRODUCTION"
	RoleQC         Role = "QC"
	RolePacking    Role = "PACKING"
	RoleShipping   Role = "SHIPPING"
	// RoleCS is customer support: they look an order up by its store order id or
	// recipient, read the full shipping details the seller uploaded, and attach
	// the tracking number they got from the carrier. They never touch production,
	// the catalog or users.
	RoleCS     Role = "CS"
	RoleSeller Role = "SELLER"
)

// AllRoles is the canonical list used by the seeder.
var AllRoles = []Role{
	RoleOwner, RoleAdmin, RoleOps, RoleDesigner,
	RoleProduction, RoleQC, RolePacking, RoleShipping, RoleCS, RoleSeller,
}

// InternalStatus is the factory-internal production status (DTWay flow):
// Pending -> Đã in -> Đã cắt -> Đã QC. It applies to order items, batches and
// batch items. Sellers never see this level of detail.
type InternalStatus string

const (
	StatusPending  InternalStatus = "PENDING"   // Pending
	StatusPrinted  InternalStatus = "PRINTED"   // Đã in
	StatusCut      InternalStatus = "CUT"       // Đã cắt
	StatusQCPassed InternalStatus = "QC_PASSED" // Đã QC
)

// internalStatusRank orders the internal statuses so we can compute the
// least-advanced status across an item's batch parts.
var internalStatusRank = map[InternalStatus]int{
	StatusPending:  0,
	StatusPrinted:  1,
	StatusCut:      2,
	StatusQCPassed: 3,
}

// Rank returns the ordinal of an internal status (higher = more advanced).
func (s InternalStatus) Rank() int { return internalStatusRank[s] }

// Valid reports whether s is a known internal status.
func (s InternalStatus) Valid() bool {
	_, ok := internalStatusRank[s]
	return ok
}

// ReviewStatus is the operational intake status of a seller-uploaded/imported
// order. New orders enter PENDING_REVIEW and only reach the design/production
// flow once an Ops/Designer approves them. It is orthogonal to SellerStatus and
// InternalStatus (which only apply once an order is APPROVED).
type ReviewStatus string

const (
	ReviewPending   ReviewStatus = "PENDING_REVIEW"   // just uploaded, awaiting operational review
	ReviewNeedsFix  ReviewStatus = "NEEDS_CORRECTION" // sent back to seller for correction
	ReviewApproved  ReviewStatus = "APPROVED"         // released into design/production
	ReviewRejected  ReviewStatus = "REJECTED"         // rejected by ops (won't be produced)
	ReviewCancelled ReviewStatus = "CANCELLED"        // cancelled (by seller or via cancellation request)
)

// reviewStatusValid is the set of known review statuses.
var reviewStatusValid = map[ReviewStatus]bool{
	ReviewPending: true, ReviewNeedsFix: true, ReviewApproved: true,
	ReviewRejected: true, ReviewCancelled: true,
}

// Valid reports whether s is a known review status.
func (s ReviewStatus) Valid() bool { return reviewStatusValid[s] }

// CancellationStatus tracks a cancellation on an order. NONE is the default;
// SELLER_CANCELLED is a direct seller cancel of a pending-review order; REQUESTED
// is a seller-submitted request awaiting an Ops/Admin decision; APPROVED/REJECTED
// are the resolved outcomes of such a request.
type CancellationStatus string

const (
	CancellationNone      CancellationStatus = "NONE"
	CancellationSeller    CancellationStatus = "SELLER_CANCELLED"
	CancellationRequested CancellationStatus = "REQUESTED"
	CancellationApproved  CancellationStatus = "APPROVED"
	CancellationRejected  CancellationStatus = "REJECTED"
)

// CancelStage snapshots how far an order (or one line item) had progressed at the
// moment a cancellation was asked for. It is the single fact that decides both
// halves of the cancellation policy: whether the seller may cancel outright or
// must wait for an Ops/Admin decision, and whether the customer is still charged.
// Work that was already started consumed material and machine time, so it is
// billed even though the order ends up cancelled.
type CancelStage string

const (
	// CancelStageNone: no cancellation has ever touched this row.
	CancelStageNone CancelStage = ""
	// CancelStagePreProduction: nothing has been produced yet (still in review, or
	// approved but no item is batched or past PENDING) — cancel is free.
	CancelStagePreProduction CancelStage = "PRE_PRODUCTION"
	// CancelStageInProduction: at least one item is batched / printed / cut / QC'd.
	CancelStageInProduction CancelStage = "IN_PRODUCTION"
	// CancelStagePacked: packed or handed off to the carrier.
	CancelStagePacked CancelStage = "PACKED"
	// CancelStageShipped: already shipped.
	CancelStageShipped CancelStage = "SHIPPED"
)

// Billable reports whether a cancellation taken at this stage is still charged to
// the customer. Only a pre-production cancellation is free.
func (s CancelStage) Billable() bool {
	switch s {
	case CancelStageInProduction, CancelStagePacked, CancelStageShipped:
		return true
	}
	return false
}

// NeedsApproval reports whether a seller cancellation at this stage has to go
// through Ops/Admin instead of taking effect immediately. It is deliberately the
// same predicate as Billable: the moment money is on the line, a human decides.
func (s CancelStage) NeedsApproval() bool { return s.Billable() }

// SellerStatus is the high-level status exposed to sellers. Sellers only ever
// see these five values, never the internal print/cut/QC steps.
type SellerStatus string

const (
	SellerStatusProduction SellerStatus = "PRODUCTION"
	SellerStatusPacked     SellerStatus = "PACKED"
	SellerStatusHandedOff  SellerStatus = "HANDED_OFF"
	SellerStatusShipped    SellerStatus = "SHIPPED"
	// SellerStatusDelivered: the carrier reported the parcel delivered.
	//
	// Reached from the tracking sync, not from a desk in the factory — nobody here
	// witnesses a delivery. Without it the timeline topped out at SHIPPED while the
	// tracking badge next to it already said "Đã giao", which read as the two
	// disagreeing about the same parcel.
	SellerStatusDelivered SellerStatus = "DELIVERED"
)

// Rank orders the lifecycle so a transition can be checked for direction. Status
// only ever moves forward: a provider that reports "in transit" after "delivered"
// (a return scan, a reused number) must not walk the order backwards.
func (s SellerStatus) Rank() int {
	switch s {
	case SellerStatusProduction:
		return 1
	case SellerStatusPacked:
		return 2
	case SellerStatusHandedOff:
		return 3
	case SellerStatusShipped:
		return 4
	case SellerStatusDelivered:
		return 5
	}
	return 0
}

// HandedOver reports whether the parcel has left the factory for the carrier.
//
// This is the boundary between the two halves of an order's life: everything up
// to it is our own production flow, everything after it belongs to the shipping
// side. The tracking integration keys off this — a parcel nobody has handed over
// yet has no journey to fetch, so we do not call the provider about it.
func (s SellerStatus) HandedOver() bool {
	return s.Rank() >= SellerStatusHandedOff.Rank()
}

// HandedOverStatuses is the SQL-friendly form of HandedOver.
var HandedOverStatuses = []string{
	string(SellerStatusHandedOff), string(SellerStatusShipped), string(SellerStatusDelivered),
}

// DesignStatus tracks the designer's progress on an item before it enters a batch.
type DesignStatus string

const (
	DesignPending    DesignStatus = "PENDING"     // chưa làm
	DesignInProgress DesignStatus = "IN_PROGRESS" // đang design
	DesignReady      DesignStatus = "READY"       // design ready
	DesignMissing    DesignStatus = "MISSING"     // thiếu mockup/asset
)

// QCResult is the outcome of a quality-control comparison against the mockup.
type QCResult string

const (
	QCPass QCResult = "PASS"
	QCFail QCResult = "FAIL"
	// QCUndo là gỡ một lần QC pass bấm nhầm: sản phẩm KHÔNG hỏng, không có tấm
	// nào bị vứt đi, chỉ là cửa QC đóng nhầm nên phải mở lại. Nó là một bản ghi
	// QC riêng (không phải FAIL) để lịch sử QC nói đúng chuyện đã xảy ra — FAIL
	// đồng nghĩa với huỷ hàng và làm lại, còn đây thì không.
	QCUndo QCResult = "UNDO"
)

// ImportJobStatus tracks the lifecycle of an order import.
type ImportJobStatus string

const (
	ImportPreview   ImportJobStatus = "PREVIEW"   // validated, awaiting commit
	ImportCommitted ImportJobStatus = "COMMITTED" // orders created
	ImportFailed    ImportJobStatus = "FAILED"
	ImportCancelled ImportJobStatus = "CANCELLED"
)

// NoteSeverity mirrors the exception catalog severities.
type NoteSeverity string

const (
	SeverityLow      NoteSeverity = "LOW"
	SeverityNormal   NoteSeverity = "NORMAL"
	SeverityHigh     NoteSeverity = "HIGH"
	SeverityCritical NoteSeverity = "CRITICAL"
)

// NoteStatus tracks a note / required-attention task lifecycle.
type NoteStatus string

const (
	NoteOpen       NoteStatus = "OPEN"
	NoteInProgress NoteStatus = "IN_PROGRESS"
	NoteWaiting    NoteStatus = "WAITING"
	NoteResolved   NoteStatus = "RESOLVED"
)

// EntityType identifies what a note / status history / audit log points to.
type EntityType string

const (
	EntityOrder     EntityType = "ORDER"
	EntityOrderItem EntityType = "ORDER_ITEM"
	EntityBatch     EntityType = "BATCH"
	EntityBatchItem EntityType = "BATCH_ITEM"
	EntityPackage   EntityType = "PACKAGE"
	EntityHandoff   EntityType = "HANDOFF"
)

// PackageStatus tracks a package through packing.
type PackageStatus string

const (
	PackageOpen   PackageStatus = "OPEN"
	PackagePacked PackageStatus = "PACKED"
)

// HandoffStatus tracks the THE handoff. SHIPPED/tracking is a later phase.
type HandoffStatus string

const (
	HandoffHandedOff HandoffStatus = "HANDED_OFF"
	HandoffShipped   HandoffStatus = "SHIPPED"
)

// Priority for production batches.
type Priority string

const (
	PriorityNormal Priority = "NORMAL"
	PriorityHigh   Priority = "HIGH"
	PriorityUrgent Priority = "URGENT"
)

// TrackingStatus is the shipment tracking state on an order. NONE means no
// tracking has been recorded yet. The other values mirror the common carrier /
// 17TRACK lifecycle so the UI can render a stable status badge whether the value
// was entered manually or synced from a provider later.
type TrackingStatus string

const (
	TrackingNone           TrackingStatus = "NONE"
	TrackingPending        TrackingStatus = "PENDING"
	TrackingPreTransit     TrackingStatus = "PRE_TRANSIT"
	TrackingInTransit      TrackingStatus = "IN_TRANSIT"
	TrackingOutForDelivery TrackingStatus = "OUT_FOR_DELIVERY"
	TrackingPickUp         TrackingStatus = "PICK_UP"
	TrackingDelivered      TrackingStatus = "DELIVERED"
	TrackingUndelivered    TrackingStatus = "UNDELIVERED"
	TrackingException      TrackingStatus = "EXCEPTION"
	TrackingExpired        TrackingStatus = "EXPIRED"
	TrackingCancelled      TrackingStatus = "CANCELLED"
)

var trackingStatusValid = map[TrackingStatus]bool{
	TrackingNone: true, TrackingPending: true, TrackingPreTransit: true,
	TrackingInTransit: true, TrackingOutForDelivery: true, TrackingPickUp: true,
	TrackingDelivered: true, TrackingUndelivered: true,
	TrackingException: true, TrackingExpired: true, TrackingCancelled: true,
}

// Valid reports whether s is a known tracking status.
func (s TrackingStatus) Valid() bool { return trackingStatusValid[s] }

// Terminal reports whether the shipment has reached a state the carrier will not
// move on from. The sync scheduler uses this to stop polling a parcel forever:
// a delivered/expired/cancelled parcel never changes again, so re-checking it
// only burns provider rate limit that a live parcel needs.
func (s TrackingStatus) Terminal() bool {
	switch s {
	case TrackingDelivered, TrackingExpired, TrackingCancelled:
		return true
	}
	return false
}

// DesignSide identifies which physical side of a product a design belongs to.
// SINGLE covers one-sided products (the default / legacy case); FRONT and BACK
// support two-sided products. Modelled as an enum (not two hard-coded columns) so
// more sides can be added later without a schema change beyond new enum values.
type DesignSide string

const (
	DesignSideSingle DesignSide = "SINGLE"
	DesignSideFront  DesignSide = "FRONT"
	DesignSideBack   DesignSide = "BACK"
)

// BatchLinkKind is the kind of production link attached to a whole batch.
type BatchLinkKind string

const (
	BatchLinkPrint BatchLinkKind = "PRINT"
	BatchLinkCut   BatchLinkKind = "CUT"
)

// Valid reports whether k is a known batch link kind.
func (k BatchLinkKind) Valid() bool { return k == BatchLinkPrint || k == BatchLinkCut }
