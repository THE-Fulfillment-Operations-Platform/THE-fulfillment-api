package services

import (
	"testing"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

func noteSvc(db *gorm.DB) *NoteService {
	repo := repositories.New(db)
	return &NoteService{repo: repo, audit: &AuditService{repo: repo}}
}

// seedScopeNotes creates one note per side of the CS line and returns their ids:
// a workshop job (owned by DESIGNER, as the import raises it) and a customer
// report (owned by CS).
func seedScopeNotes(t *testing.T, db *gorm.DB) (workshopID, csID uint) {
	t.Helper()
	workshop := &models.Note{
		Title: "Thiếu Mockup URL", ReasonCode: "ART_MISSING", Severity: models.SeverityHigh,
		Status: models.NoteOpen, IsRequiredAttention: true,
		EntityType: models.EntityOrderItem, OwnerRole: models.RoleDesigner,
	}
	cs := &models.Note{
		Title: "Khách báo giao sai địa chỉ", Severity: models.SeverityHigh,
		Status: models.NoteOpen, IsRequiredAttention: true,
		EntityType: models.EntityOrder, OwnerRole: models.RoleCS,
	}
	for _, n := range []*models.Note{workshop, cs} {
		if err := db.Create(n).Error; err != nil {
			t.Fatalf("seed note: %v", err)
		}
	}
	return workshop.ID, cs.ID
}

// TestNotes_CSSeesOnlyItsOwnQueue: customer support's notes screen is a different
// list from everyone else's. CS handles what a customer reported against an order
// — never the workshop's production problems, which each already name the role
// that must fix them.
func TestNotes_CSSeesOnlyItsOwnQueue(t *testing.T) {
	db := newImportDB(t)
	svc := noteSvc(db)
	workshopID, csID := seedScopeNotes(t, db)

	csActor := Actor{ID: 30, Role: models.RoleCS}
	opsActor := Actor{ID: 1, Role: models.RoleOps}

	// List: CS gets exactly their own note; Ops still sees both queues.
	rows, total, err := svc.List(csActor, repositories.NoteFilter{})
	if err != nil {
		t.Fatalf("cs list: %v", err)
	}
	if total != 1 || len(rows) != 1 || rows[0].ID != csID {
		t.Fatalf("cs list = %d rows (total %d), want only note %d", len(rows), total, csID)
	}
	if _, total, err = svc.List(opsActor, repositories.NoteFilter{}); err != nil || total != 2 {
		t.Fatalf("ops list total = %d (err %v), want 2", total, err)
	}

	// A CS user must not reach a workshop note by typing its id either.
	if _, err := svc.Get(csActor, workshopID); err == nil {
		t.Fatal("CS should not be able to open a workshop note by id")
	}
	if _, err := svc.Get(csActor, csID); err != nil {
		t.Fatalf("CS should be able to open their own note: %v", err)
	}
	if _, err := svc.Get(opsActor, workshopID); err != nil {
		t.Fatalf("ops should be able to open a workshop note: %v", err)
	}
}

// TestNotes_CSCannotDeleteWorkshopQueue: the scoping has to hold on the delete
// paths too, and "chọn tất cả" is the dangerous one — it deletes by FILTER, so an
// unscoped filter would let CS wipe the workshop's inbox in one click.
func TestNotes_CSCannotDeleteWorkshopQueue(t *testing.T) {
	db := newImportDB(t)
	svc := noteSvc(db)
	workshopID, csID := seedScopeNotes(t, db)
	csActor := Actor{ID: 30, Role: models.RoleCS}

	if _, err := svc.DeleteNotes(csActor, []uint{workshopID}); err == nil {
		t.Fatal("CS deleting a workshop note by id should be refused")
	}

	n, err := svc.DeleteNotesMatching(csActor, repositories.NoteFilter{})
	if err != nil {
		t.Fatalf("cs delete-all: %v", err)
	}
	if n != 1 {
		t.Fatalf("cs 'xoá tất cả' removed %d notes, want 1 (only their own)", n)
	}
	var left models.Note
	if err := db.First(&left, workshopID).Error; err != nil {
		t.Fatalf("workshop note must survive CS 'xoá tất cả': %v", err)
	}
	if err := db.First(&models.Note{}, csID).Error; err == nil {
		t.Fatal("CS's own note should have been deleted")
	}
}

// TestNotes_CSCreatedNoteLandsInCSQueue: the form's "Phụ trách" is optional, so a
// CS user logging a customer report without touching it would otherwise create a
// note owned by nobody — and never see it again, since their list is scoped.
func TestNotes_CSCreatedNoteLandsInCSQueue(t *testing.T) {
	db := newImportDB(t)
	svc := noteSvc(db)
	csActor := Actor{ID: 30, Role: models.RoleCS}

	n, err := svc.Create(csActor, NoteInput{Title: "Khách báo thiếu hàng"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if n.OwnerRole != models.RoleCS {
		t.Fatalf("owner_role = %q, want CS", n.OwnerRole)
	}
	rows, _, err := svc.List(csActor, repositories.NoteFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != n.ID {
		t.Fatalf("CS should see the note they just filed, got %d rows", len(rows))
	}

	// An internal role's note is untouched: no owner is still no owner, because
	// their queues are not scoped and the field means "assigned to", not "mine".
	ops, err := svc.Create(Actor{ID: 1, Role: models.RoleOps}, NoteInput{Title: "Ghi chú nội bộ"})
	if err != nil {
		t.Fatalf("ops create: %v", err)
	}
	if ops.OwnerRole != "" {
		t.Fatalf("ops note owner_role = %q, want empty", ops.OwnerRole)
	}
}
