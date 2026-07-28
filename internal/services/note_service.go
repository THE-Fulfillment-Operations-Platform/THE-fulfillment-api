package services

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/apptypes"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// NoteService manages internal notes / required-attention tasks.
type NoteService struct {
	repo  *repositories.Repositories
	audit *AuditService
}

// NoteInput creates or updates a note. Severity/status default sensibly. Title is
// NOT bound as required here because the SAME struct backs partial updates (e.g.
// "resolve" sends only status + is_required_attention); Create validates title in
// the service instead so a partial update isn't rejected for a missing title.
type NoteInput struct {
	Title               string              `json:"title"`
	Body                string              `json:"body"`
	ReasonCode          string              `json:"reason_code"`
	Severity            models.NoteSeverity `json:"severity"`
	Status              models.NoteStatus   `json:"status"`
	IsRequiredAttention *bool               `json:"is_required_attention"`
	EntityType          models.EntityType   `json:"entity_type"`
	EntityID            *uint               `json:"entity_id"`
	OwnerRole           models.Role         `json:"owner_role"`
	AssignedToID        *uint               `json:"assigned_to_id"`
	DueDate             *apptypes.Date      `json:"due_date"`
	Resolution          string              `json:"resolution"`
}

func (s *NoteService) Create(actor Actor, in NoteInput) (*models.Note, error) {
	if strings.TrimSpace(in.Title) == "" {
		return nil, apperr.BadRequest("Tiêu đề ghi chú là bắt buộc")
	}
	severity := in.Severity
	if severity == "" {
		severity = models.SeverityNormal
	}
	status := in.Status
	if status == "" {
		status = models.NoteOpen
	}
	ra := false
	if in.IsRequiredAttention != nil {
		ra = *in.IsRequiredAttention
	}
	n := &models.Note{
		Title: in.Title, Body: in.Body, ReasonCode: in.ReasonCode, Severity: severity, Status: status,
		IsRequiredAttention: ra, EntityType: in.EntityType, EntityID: in.EntityID, OwnerRole: in.OwnerRole,
		AssignedToID: in.AssignedToID, DueDate: in.DueDate.TimePtr(), CreatedByID: actor.IDPtr(),
	}
	if err := s.repo.Note.Create(n); err != nil {
		return nil, apperr.Internal("could not create note").Wrap(err)
	}
	s.audit.Log(actor, "NOTE_CREATE", "note", &n.ID, "Created note: "+n.Title, nil)
	return n, nil
}

func (s *NoteService) Get(id uint) (*models.Note, error) {
	n, err := s.repo.Note.FindByID(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperr.NotFound("Note not found")
		}
		return nil, apperr.Internal("lookup failed").Wrap(err)
	}
	return n, nil
}

func (s *NoteService) List(f repositories.NoteFilter) ([]models.Note, int64, error) {
	f.Page = f.Page.Normalize()
	return s.repo.Note.List(f)
}

func (s *NoteService) Update(actor Actor, id uint, in NoteInput) (*models.Note, error) {
	n, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if in.Title != "" {
		n.Title = in.Title
	}
	n.Body = in.Body
	if in.ReasonCode != "" {
		n.ReasonCode = in.ReasonCode
	}
	if in.Severity != "" {
		n.Severity = in.Severity
	}
	if in.Status != "" {
		n.Status = in.Status
		if in.Status == models.NoteResolved && n.ResolvedAt == nil {
			now := time.Now()
			n.ResolvedAt = &now
			n.ResolvedByID = actor.IDPtr()
		}
	}
	if in.IsRequiredAttention != nil {
		n.IsRequiredAttention = *in.IsRequiredAttention
	}
	if in.OwnerRole != "" {
		n.OwnerRole = in.OwnerRole
	}
	if in.AssignedToID != nil {
		n.AssignedToID = in.AssignedToID
	}
	if in.DueDate != nil {
		n.DueDate = in.DueDate.TimePtr()
	}
	if in.Resolution != "" {
		n.Resolution = in.Resolution
	}
	if err := s.repo.Note.Update(n); err != nil {
		return nil, apperr.Internal("could not update note").Wrap(err)
	}
	s.audit.Log(actor, "NOTE_UPDATE", "note", &n.ID, "Updated note: "+n.Title, nil)
	return n, nil
}

// NoteDeleteResult reports what a (bulk) note delete actually did.
type NoteDeleteResult struct {
	DeletedIDs []uint `json:"deleted_ids"`
	// Ids that were already gone. Notes have no in-use guard — nothing references
	// a note — so this is the only reason a delete skips one.
	MissingIDs []uint `json:"missing_ids"`
}

// DeleteNotes removes many notes in one request: a couple of statements for the
// whole selection instead of the three per note (read, delete, audit) that an
// id-at-a-time API costs. A screen full of auto-generated warnings is exactly the
// case this exists for.
func (s *NoteService) DeleteNotes(actor Actor, ids []uint) (*NoteDeleteResult, error) {
	clean := dedupeIDs(ids)
	if len(clean) == 0 {
		return nil, apperr.BadRequest("Chưa chọn ghi chú nào để xoá")
	}
	if len(clean) > maxDeleteIDs {
		return nil, apperr.BadRequest(fmt.Sprintf("Chỉ xoá tối đa %d ghi chú mỗi lần", maxDeleteIDs))
	}

	found, err := s.repo.Note.ListByIDs(clean)
	if err != nil {
		return nil, apperr.Internal("lookup failed").Wrap(err)
	}
	exists := make(map[uint]bool, len(found))
	for _, n := range found {
		exists[n.ID] = true
	}

	res := &NoteDeleteResult{DeletedIDs: []uint{}, MissingIDs: []uint{}}
	for _, id := range clean {
		if exists[id] {
			res.DeletedIDs = append(res.DeletedIDs, id)
		} else {
			res.MissingIDs = append(res.MissingIDs, id)
		}
	}
	if len(res.DeletedIDs) > 0 {
		if _, err := s.repo.Note.DeleteMany(res.DeletedIDs); err != nil {
			return nil, apperr.Internal("could not delete notes").Wrap(err)
		}
	}

	// One audit entry per action, not per row.
	if len(res.DeletedIDs) == 1 {
		id := res.DeletedIDs[0]
		s.audit.Log(actor, "NOTE_DELETE", "note", &id, "Deleted note", nil)
	} else if len(res.DeletedIDs) > 1 {
		s.audit.Log(actor, "NOTE_DELETE_BULK", "note", nil,
			fmt.Sprintf("Deleted %d notes", len(res.DeletedIDs)), nil)
	}
	return res, nil
}

// DeleteNotesMatching removes EVERY note matching the filter — what the screen's
// "chọn tất cả" does. The client sends the filter it is looking at, not a list of
// ids: the set can be tens of thousands of rows, and naming them one by one would
// mean either a huge request or a silent cap at whatever the client managed to
// load. Returns how many rows were actually removed.
func (s *NoteService) DeleteNotesMatching(actor Actor, f repositories.NoteFilter) (int64, error) {
	// Pagination is meaningless here — the action covers the whole match.
	f.Page = repositories.Page{}
	n, err := s.repo.Note.DeleteByFilter(f)
	if err != nil {
		return 0, apperr.Internal("could not delete notes").Wrap(err)
	}
	s.audit.Log(actor, "NOTE_DELETE_BULK", "note", nil,
		fmt.Sprintf("Deleted %d notes matching filter (%s)", n, describeNoteFilter(f)), nil)
	return n, nil
}

// describeNoteFilter renders the filter for the audit trail, so the log says what
// a bulk delete actually covered rather than just a number.
func describeNoteFilter(f repositories.NoteFilter) string {
	parts := []string{}
	if f.Status != "" {
		parts = append(parts, "status="+f.Status)
	}
	if f.Severity != "" {
		parts = append(parts, "severity="+f.Severity)
	}
	if f.EntityType != "" {
		parts = append(parts, "entity_type="+f.EntityType)
	}
	if f.EntityID != nil {
		parts = append(parts, fmt.Sprintf("entity_id=%d", *f.EntityID))
	}
	if f.RequiredAttention != nil {
		parts = append(parts, fmt.Sprintf("required_attention=%t", *f.RequiredAttention))
	}
	if len(parts) == 0 {
		return "no filter — all notes"
	}
	return strings.Join(parts, ", ")
}

// CountNotesMatching reports the size of a filter's match, so the UI can offer
// (and confirm) "select all N" with the real number rather than a page count.
func (s *NoteService) CountNotesMatching(f repositories.NoteFilter) (int64, error) {
	f.Page = repositories.Page{}
	n, err := s.repo.Note.CountByFilter(f)
	if err != nil {
		return 0, apperr.Internal("lookup failed").Wrap(err)
	}
	return n, nil
}

func (s *NoteService) Delete(actor Actor, id uint) error {
	res, err := s.DeleteNotes(actor, []uint{id})
	if err != nil {
		return err
	}
	if len(res.DeletedIDs) == 0 {
		return apperr.NotFound("Note not found")
	}
	return nil
}
