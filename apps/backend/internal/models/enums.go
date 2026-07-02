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
	RoleSeller     Role = "SELLER"
)

// AllRoles is the canonical list used by the seeder.
var AllRoles = []Role{
	RoleOwner, RoleAdmin, RoleOps, RoleDesigner,
	RoleProduction, RoleQC, RolePacking, RoleShipping, RoleSeller,
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

// SellerStatus is the high-level status exposed to sellers. Sellers only ever
// see these four values, never the internal print/cut/QC steps.
type SellerStatus string

const (
	SellerStatusProduction SellerStatus = "PRODUCTION"
	SellerStatusPacked     SellerStatus = "PACKED"
	SellerStatusHandedOff  SellerStatus = "HANDED_OFF"
	SellerStatusShipped    SellerStatus = "SHIPPED"
)

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
