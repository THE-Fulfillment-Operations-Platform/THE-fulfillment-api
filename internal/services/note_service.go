package services

import (
	"errors"
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

func (s *NoteService) Delete(actor Actor, id uint) error {
	if _, err := s.Get(id); err != nil {
		return err
	}
	if err := s.repo.Note.Delete(id); err != nil {
		return apperr.Internal("could not delete note").Wrap(err)
	}
	s.audit.Log(actor, "NOTE_DELETE", "note", &id, "Deleted note", nil)
	return nil
}
