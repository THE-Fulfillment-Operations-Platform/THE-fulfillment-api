package services

import (
	"strings"
	"testing"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

func newUserFixture(t *testing.T) (*gorm.DB, *UserService) {
	t.Helper()
	db := newSplitDB(t)
	repo := repositories.New(db)
	return db, &UserService{repo: repo, audit: &AuditService{repo: repo}}
}

func seedUser(t *testing.T, db *gorm.DB, email string, role models.Role) *models.User {
	t.Helper()
	u := &models.User{
		Email: email, PasswordHash: "x", FullName: "Test " + email,
		Role: role, IsActive: true,
	}
	if err := db.Create(u).Error; err != nil {
		t.Fatalf("seed user %s: %v", email, err)
	}
	return u
}

// TestUserDelete_SoftDeletesAndAudits: xoá người dùng là xoá MỀM — mọi dấu vết
// người đó để lại (audit, batch đã tạo, lượt QC) vẫn trỏ về đúng một con người,
// nên hàng xoá cứng là không được. Người bị xoá biến khỏi danh sách và không
// đăng nhập lại được.
func TestUserDelete_SoftDeletesAndAudits(t *testing.T) {
	db, svc := newUserFixture(t)
	seedUser(t, db, "owner@the.local", models.RoleOwner)
	victim := seedUser(t, db, "ops@the.local", models.RoleOps)
	actor := Actor{ID: 1, Email: "owner@the.local", Role: models.RoleOwner}

	if err := svc.Delete(actor, victim.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := svc.Get(victim.ID); err == nil {
		t.Fatalf("người dùng đã xoá không được tra ra nữa")
	}
	// Hàng vẫn nằm trong bảng (soft delete), chỉ bị đánh dấu.
	var row models.User
	if err := db.Unscoped().First(&row, victim.ID).Error; err != nil {
		t.Fatalf("hàng phải còn để giữ truy vết: %v", err)
	}
	if !row.DeletedAt.Valid {
		t.Fatalf("want soft-deleted, deleted_at vẫn trống")
	}
	var logs int64
	db.Model(&models.AuditLog{}).Where("action = ?", "USER_DELETE").Count(&logs)
	if logs != 1 {
		t.Fatalf("want 1 audit USER_DELETE, got %d", logs)
	}
}

// TestUserDelete_CannotDeleteSelf: tự xoá tài khoản đang đăng nhập là tự khoá
// mình ra ngoài giữa ca — và nếu đó là OWNER cuối thì khoá cả nhà máy.
func TestUserDelete_CannotDeleteSelf(t *testing.T) {
	db, svc := newUserFixture(t)
	owner := seedUser(t, db, "owner@the.local", models.RoleOwner)
	seedUser(t, db, "owner2@the.local", models.RoleOwner) // để không vướng luật OWNER cuối

	err := svc.Delete(Actor{ID: owner.ID, Email: owner.Email, Role: models.RoleOwner}, owner.ID)
	if err == nil {
		t.Fatalf("tự xoá chính mình phải bị chặn")
	}
	if _, err := svc.Get(owner.ID); err != nil {
		t.Fatalf("tài khoản phải còn nguyên sau khi bị chặn: %v", err)
	}
}

// TestUserDelete_CannotDeleteLastOwner: OWNER là cổng của toàn hệ thống (đặt
// định mức, quản trị người dùng, hạ trạng thái sản xuất). Xoá OWNER cuối cùng
// là khoá vĩnh viễn những việc đó, không ai mở lại được từ trong ứng dụng.
func TestUserDelete_CannotDeleteLastOwner(t *testing.T) {
	db, svc := newUserFixture(t)
	owner := seedUser(t, db, "owner@the.local", models.RoleOwner)
	admin := seedUser(t, db, "admin@the.local", models.RoleAdmin)
	actor := Actor{ID: admin.ID, Email: admin.Email, Role: models.RoleAdmin}

	if err := svc.Delete(actor, owner.ID); err == nil {
		t.Fatalf("xoá OWNER cuối cùng phải bị chặn")
	}
	if _, err := svc.Get(owner.ID); err != nil {
		t.Fatalf("OWNER phải còn nguyên: %v", err)
	}

	// Có OWNER thứ hai thì OWNER mới được xoá bớt một.
	seedUser(t, db, "owner2@the.local", models.RoleOwner)
	ownerActor := Actor{ID: 99, Email: "owner2@the.local", Role: models.RoleOwner}
	if err := svc.Delete(ownerActor, owner.ID); err != nil {
		t.Fatalf("còn OWNER khác thì xoá được: %v", err)
	}
}

// TestUserDelete_AdminCannotDeleteOwner: ADMIN quản trị được người dùng thường
// nhưng không được gỡ chủ sở hữu — nếu không, một ADMIN có thể tự dọn sạch lớp
// giám sát phía trên mình.
func TestUserDelete_AdminCannotDeleteOwner(t *testing.T) {
	db, svc := newUserFixture(t)
	owner := seedUser(t, db, "owner@the.local", models.RoleOwner)
	seedUser(t, db, "owner2@the.local", models.RoleOwner) // vẫn còn OWNER khác
	admin := seedUser(t, db, "admin@the.local", models.RoleAdmin)

	err := svc.Delete(Actor{ID: admin.ID, Email: admin.Email, Role: models.RoleAdmin}, owner.ID)
	if err == nil {
		t.Fatalf("ADMIN không được xoá OWNER")
	}
	if _, err := svc.Get(owner.ID); err != nil {
		t.Fatalf("OWNER phải còn nguyên: %v", err)
	}
}

// TestUserCreate_DeletedEmailNeedsExplicitRestore: email của một tài khoản ĐÃ
// XOÁ không được âm thầm hồi sinh. Khôi phục là gán lại toàn bộ lịch sử của
// người cũ (batch đã tạo, lượt QC, nhật ký) cho tài khoản đang tạo — nếu email
// công ty đã chuyển sang người khác thì đó là gán nhầm việc cho nhầm người. Nên
// lần tạo đầu bị chặn bằng một mã lỗi ổn định để giao diện hỏi lại, KHÔNG phải
// lỗi 500 do đâm unique index như trước khi vá.
func TestUserCreate_DeletedEmailNeedsExplicitRestore(t *testing.T) {
	db, svc := newUserFixture(t)
	seedUser(t, db, "owner@the.local", models.RoleOwner)
	actor := Actor{ID: 1, Email: "owner@the.local", Role: models.RoleOwner}

	created, err := svc.Create(actor, CreateUserInput{
		Email: "ops@the.local", Password: "Password123!", FullName: "Ops cũ", Role: models.RoleOps,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.Delete(actor, created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	_, err = svc.Create(actor, CreateUserInput{
		Email: "ops@the.local", Password: "Password456!", FullName: "Ops mới", Role: models.RoleQC,
	})
	if err == nil {
		t.Fatalf("tạo trùng email đã xoá phải bị chặn để người dùng xác nhận")
	}
	ae, ok := apperr.As(err)
	if !ok || ae.Code != "USER_DELETED_EMAIL" {
		t.Fatalf("cần mã lỗi ổn định USER_DELETED_EMAIL cho FE nhận biết, got %+v", err)
	}
	// Message phải nói rõ tài khoản cũ là ai để người vận hành quyết được.
	if !strings.Contains(ae.Message, "Ops cũ") {
		t.Fatalf("message phải nêu tên tài khoản đã xoá, got %q", ae.Message)
	}
	// Chưa có gì được ghi: vẫn đúng một hàng, vẫn đang bị xoá.
	var rows []models.User
	db.Unscoped().Where("email = ?", "ops@the.local").Find(&rows)
	if len(rows) != 1 || !rows[0].DeletedAt.Valid {
		t.Fatalf("lần tạo bị chặn không được đụng dữ liệu, got %+v", rows)
	}
}

// TestUserCreate_RestoreWhenConfirmed: người dùng xác nhận (RestoreDeleted) thì
// hệ thống khôi phục CHÍNH tài khoản cũ — giữ nguyên id nên mọi lịch sử vẫn trỏ
// đúng người — và ghi đè thông tin mới.
func TestUserCreate_RestoreWhenConfirmed(t *testing.T) {
	db, svc := newUserFixture(t)
	seedUser(t, db, "owner@the.local", models.RoleOwner)
	actor := Actor{ID: 1, Email: "owner@the.local", Role: models.RoleOwner}

	created, err := svc.Create(actor, CreateUserInput{
		Email: "ops@the.local", Password: "Password123!", FullName: "Ops cũ", Role: models.RoleOps,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.Delete(actor, created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	again, err := svc.Create(actor, CreateUserInput{
		Email: "ops@the.local", Password: "Password456!", FullName: "Ops mới", Role: models.RoleQC,
		RestoreDeleted: true,
	})
	if err != nil {
		t.Fatalf("khôi phục có xác nhận phải chạy được, got: %v", err)
	}
	if again.ID != created.ID {
		t.Fatalf("phải khôi phục bản ghi cũ (id %d), got id %d", created.ID, again.ID)
	}
	if again.FullName != "Ops mới" || again.Role != models.RoleQC {
		t.Fatalf("thông tin mới phải được ghi đè: %+v", again)
	}
	if again.PasswordHash == created.PasswordHash {
		t.Fatalf("mật khẩu mới phải thay mật khẩu cũ")
	}
	// Đúng một hàng cho email đó, và nó đang sống.
	var rows []models.User
	if err := db.Unscoped().Where("email = ?", "ops@the.local").Find(&rows).Error; err != nil {
		t.Fatalf("load rows: %v", err)
	}
	if len(rows) != 1 || rows[0].DeletedAt.Valid {
		t.Fatalf("want đúng 1 hàng còn sống, got %d (%+v)", len(rows), rows)
	}
	if _, err := svc.Get(again.ID); err != nil {
		t.Fatalf("user khôi phục phải tra ra được: %v", err)
	}
}

// TestUserCreate_DuplicateLiveEmailStillRejected: khôi phục chỉ áp dụng cho
// hàng ĐÃ XOÁ — email đang có người dùng thật vẫn bị từ chối như cũ.
func TestUserCreate_DuplicateLiveEmailStillRejected(t *testing.T) {
	db, svc := newUserFixture(t)
	seedUser(t, db, "ops@the.local", models.RoleOps)
	actor := Actor{ID: 1, Email: "owner@the.local", Role: models.RoleOwner}

	if _, err := svc.Create(actor, CreateUserInput{
		Email: "ops@the.local", Password: "Password123!", FullName: "Trùng", Role: models.RoleQC,
	}); err == nil {
		t.Fatalf("email đang dùng phải bị từ chối")
	}
}

// TestUserDelete_NotFound: xoá một id không tồn tại trả lỗi rõ ràng, không phải
// "thành công" im lặng.
func TestUserDelete_NotFound(t *testing.T) {
	db, svc := newUserFixture(t)
	seedUser(t, db, "owner@the.local", models.RoleOwner)
	if err := svc.Delete(Actor{ID: 1, Role: models.RoleOwner}, 999999); err == nil {
		t.Fatalf("xoá id không tồn tại phải báo lỗi")
	}
}
