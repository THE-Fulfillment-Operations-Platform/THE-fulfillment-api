package models

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"sort"
	"strings"
)

// Per-user permissions. A permission is "<feature>.<level>": the feature is one
// screen of the internal app, the level is "view" (see it) or "manage" (act on
// it). manage implies view.
//
// The role is only the DEFAULT set of ticks: a user whose Permissions column is
// NULL gets exactly their role's defaults, so existing accounts behave as before
// and pick up any later change to those defaults. Once an admin customises the
// ticks, the explicit list is stored and the role no longer widens or narrows it.
//
// What is NOT a permission, on purpose, and stays with the role:
//   - OWNER always has everything — nobody can lock the owner out.
//   - SELLER lives in the seller portal; none of this applies to them.
//   - User management and the settings/reset page (OWNER/ADMIN, OWNER): ticking
//     "manage users" would let anyone grant themselves the rest.
//   - Destructive or override actions inside a screen (delete an order or a
//     material, undo a QC pass, lower a batch status, OWNER-only quotas): these
//     need the screen's manage tick AND the role, as before.
const (
	LevelView   = "view"
	LevelManage = "manage"
)

// Feature keys — one per screen in the internal sidebar, plus batch scrap, which
// is an action on the batch screen that belongs to a different team (the people
// standing next to the ruined sheet) than the one that creates batches.
const (
	FeatDashboard     = "dashboard"
	FeatCS            = "cs"
	FeatMasterData    = "master_data"
	FeatImport        = "import"
	FeatReview        = "review"
	FeatOrders        = "orders"
	FeatCancellations = "cancellations"
	FeatDesign        = "design"
	FeatBatches       = "batches"
	FeatBatchScrap    = "batch_scrap"
	FeatProduction    = "production"
	FeatQC            = "qc"
	FeatQCResults     = "qc_results"
	FeatShipQueue     = "ship_queue"
	FeatJourneys      = "journeys"
	FeatNotes         = "notes"
	FeatAudit         = "audit"
)

// Perm builds a permission key.
func Perm(feature, level string) string { return feature + "." + level }

// View / Manage are shorthands for Perm(feature, level).
func View(feature string) string   { return Perm(feature, LevelView) }
func Manage(feature string) string { return Perm(feature, LevelManage) }

// Feature describes one tickable screen for the user form.
type Feature struct {
	Key    string   `json:"key"`
	Label  string   `json:"label"`
	Levels []string `json:"levels"`
	// ManageHint says what "Thao tác" unlocks, shown next to the tick.
	ManageHint string `json:"manage_hint,omitempty"`
}

var both = []string{LevelView, LevelManage}
var viewOnly = []string{LevelView}
var manageOnly = []string{LevelManage}

// Features is the catalog, in sidebar order.
var Features = []Feature{
	{Key: FeatDashboard, Label: "Tổng quan", Levels: viewOnly},
	{Key: FeatCS, Label: "Tra cứu CS", Levels: both, ManageHint: "gắn / đồng bộ mã vận đơn"},
	{Key: FeatMasterData, Label: "Master Data", Levels: both, ManageHint: "thêm, sửa, import NVL / SKU / seller"},
	{Key: FeatImport, Label: "Import đơn", Levels: both, ManageHint: "tải file đơn lên hệ thống"},
	{Key: FeatReview, Label: "Chờ duyệt", Levels: both, ManageHint: "duyệt, từ chối, yêu cầu sửa đơn"},
	{Key: FeatOrders, Label: "Đơn hàng / Sản phẩm", Levels: both, ManageHint: "sửa, huỷ đơn"},
	{Key: FeatCancellations, Label: "Yêu cầu huỷ", Levels: both, ManageHint: "duyệt / từ chối yêu cầu huỷ"},
	{Key: FeatDesign, Label: "Chờ thiết kế", Levels: both, ManageHint: "cập nhật design, set sẵn sàng"},
	{Key: FeatBatches, Label: "Batch sản xuất", Levels: both, ManageHint: "gom, xoá batch, gắn link in/cắt"},
	{Key: FeatBatchScrap, Label: "Huỷ batch in/cắt hỏng", Levels: manageOnly, ManageHint: "ghi nhận tấm hỏng, trả sản phẩm về làm lại"},
	{Key: FeatProduction, Label: "Bảng sản xuất", Levels: both, ManageHint: "chuyển chặng in / cắt"},
	{Key: FeatQC, Label: "Quét QC", Levels: both, ManageHint: "bấm Pass / Fail (Xem = chỉ quét để tra)"},
	{Key: FeatQCResults, Label: "Kết quả QC", Levels: viewOnly},
	{Key: FeatShipQueue, Label: "Chờ gửi hàng", Levels: both, ManageHint: "quét / gửi đơn cho THE"},
	{Key: FeatJourneys, Label: "Hành trình đơn hàng", Levels: both, ManageHint: "gắn, import, đồng bộ tracking"},
	{Key: FeatNotes, Label: "Ghi chú / Cần xử lý", Levels: both, ManageHint: "tạo, sửa, xoá ghi chú"},
	{Key: FeatAudit, Label: "Nhật ký hoạt động", Levels: viewOnly},
}

var knownPerms = func() map[string]bool {
	m := map[string]bool{}
	for _, f := range Features {
		for _, l := range f.Levels {
			m[Perm(f.Key, l)] = true
		}
	}
	return m
}()

// IsKnownPerm reports whether p is a permission in the catalog.
func IsKnownPerm(p string) bool { return knownPerms[p] }

func manageAll(features ...string) []string {
	out := make([]string, 0, len(features))
	for _, f := range features {
		out = append(out, Manage(f))
	}
	return out
}

// roleDefaults mirrors what each role could reach before per-user permissions
// existed, so NULL-permission accounts see no change.
var roleDefaults = map[Role][]string{
	RoleOps: append([]string{View(FeatDashboard), View(FeatQCResults)},
		manageAll(FeatCS, FeatMasterData, FeatImport, FeatReview, FeatOrders, FeatCancellations,
			FeatDesign, FeatBatches, FeatBatchScrap, FeatProduction, FeatQC, FeatShipQueue,
			FeatJourneys, FeatNotes)...),
	RoleDesigner: append([]string{View(FeatDashboard), View(FeatOrders), View(FeatQCResults)},
		manageAll(FeatReview, FeatDesign, FeatBatches, FeatProduction, FeatNotes)...),
	RoleProduction: append([]string{View(FeatDashboard), View(FeatOrders), View(FeatBatches), View(FeatQCResults)},
		manageAll(FeatProduction, FeatBatchScrap, FeatNotes)...),
	RoleQC: append([]string{View(FeatDashboard), View(FeatOrders), View(FeatBatches), View(FeatQCResults)},
		manageAll(FeatQC, FeatBatchScrap, FeatNotes)...),
	RolePacking: append([]string{View(FeatDashboard), View(FeatOrders), View(FeatBatches), View(FeatQCResults)},
		manageAll(FeatShipQueue, FeatJourneys, FeatNotes)...),
	RoleShipping: append([]string{View(FeatDashboard), View(FeatOrders), View(FeatBatches), View(FeatQCResults)},
		manageAll(FeatShipQueue, FeatJourneys, FeatNotes)...),
	RoleCS: manageAll(FeatCS, FeatJourneys, FeatNotes),
}

// allPerms is every permission in the catalog (OWNER, ADMIN default).
func allPerms() []string {
	out := make([]string, 0, len(knownPerms))
	for p := range knownPerms {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// RoleDefaultPerms is the tick set a role starts with, normalised.
func RoleDefaultPerms(role Role) []string {
	switch role {
	case RoleOwner, RoleAdmin:
		return allPerms()
	case RoleSeller:
		return []string{}
	}
	return NormalizePerms(roleDefaults[role])
}

// NormalizePerms drops unknown keys and duplicates, adds the view that every
// manage implies, and sorts — the canonical form stored and returned.
func NormalizePerms(in []string) []string {
	set := map[string]bool{}
	for _, p := range in {
		p = strings.TrimSpace(p)
		if !knownPerms[p] {
			continue
		}
		set[p] = true
		if feature, level, ok := strings.Cut(p, "."); ok && level == LevelManage && knownPerms[View(feature)] {
			set[View(feature)] = true
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// EffectivePerms is what a user can actually do: everything for OWNER, the
// stored list when customised, otherwise the role defaults.
func EffectivePerms(role Role, stored PermList) []string {
	if role == RoleOwner {
		return allPerms()
	}
	if role == RoleSeller {
		return []string{}
	}
	if stored == nil {
		return RoleDefaultPerms(role)
	}
	return NormalizePerms(stored)
}

// PermList is the users.permissions column: a JSON array of permission keys, or
// NULL for "use the role defaults". nil and empty are different on purpose —
// empty means the admin unticked everything.
type PermList []string

// Value implements driver.Valuer.
func (p PermList) Value() (driver.Value, error) {
	if p == nil {
		return nil, nil
	}
	b, err := json.Marshal([]string(p))
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// Scan implements sql.Scanner.
func (p *PermList) Scan(value interface{}) error {
	var raw []byte
	switch v := value.(type) {
	case nil:
		*p = nil
		return nil
	case []byte:
		raw = v
	case string:
		raw = []byte(v)
	default:
		return errors.New("PermList: unsupported scan type")
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	if out == nil {
		out = []string{}
	}
	*p = out
	return nil
}

// GormDataType tells GORM the column type.
func (PermList) GormDataType() string { return "jsonb" }

// Access is who the caller is right now, read fresh from the database (not from
// the token): role and permissions changes apply on the next request, and a
// locked or deleted account stops working at once.
type Access struct {
	UserID   uint
	Role     Role
	SellerID *uint
	Perms    map[string]bool
}

// NewAccess builds an Access for a user.
func NewAccess(u *User) *Access {
	a := &Access{UserID: u.ID, Role: u.Role, SellerID: u.SellerID, Perms: map[string]bool{}}
	for _, p := range EffectivePerms(u.Role, u.Permissions) {
		a.Perms[p] = true
	}
	return a
}

// Can reports whether the caller holds perm. OWNER holds everything.
func (a *Access) Can(perm string) bool {
	if a == nil {
		return false
	}
	return a.Role == RoleOwner || a.Perms[perm]
}
