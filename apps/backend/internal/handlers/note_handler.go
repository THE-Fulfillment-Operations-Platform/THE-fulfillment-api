package handlers

import (
	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/repositories"
	"the-fulfillment/backend/internal/response"
	"the-fulfillment/backend/internal/services"
)

// CreateNote creates a note / required-attention task. POST /api/notes
func (h *Handlers) CreateNote(c *gin.Context) {
	var in services.NoteInput
	if !bindJSON(c, &in) {
		return
	}
	n, err := h.svc.Note.Create(actor(c), in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, n)
}

// ListNotes lists notes with filters (status, severity, entity, required attention).
// GET /api/notes
func (h *Handlers) ListNotes(c *gin.Context) {
	p := pageFrom(c)
	var ra *bool
	if v := c.Query("required_attention"); v != "" {
		b := v == "true" || v == "1"
		ra = &b
	}
	f := repositories.NoteFilter{
		Page:              p,
		Status:            c.Query("status"),
		Severity:          c.Query("severity"),
		EntityType:        c.Query("entity_type"),
		EntityID:          uintQueryPtr(c, "entity_id"),
		RequiredAttention: ra,
	}
	rows, total, err := h.svc.Note.List(f)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.List(c, rows, metaFor(p, total))
}

// GetNote fetches a note. GET /api/notes/:id
func (h *Handlers) GetNote(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	n, err := h.svc.Note.Get(id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, n)
}

// UpdateNote updates a note. PUT /api/notes/:id
func (h *Handlers) UpdateNote(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	var in services.NoteInput
	if !bindJSON(c, &in) {
		return
	}
	n, err := h.svc.Note.Update(actor(c), id, in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, n)
}

// DeleteNote deletes a note. DELETE /api/notes/:id
func (h *Handlers) DeleteNote(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	if err := h.svc.Note.Delete(actor(c), id); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"deleted": true})
}

// ListAuditLogs lists audit entries (admin/owner). GET /api/audit-logs
func (h *Handlers) ListAuditLogs(c *gin.Context) {
	p := pageFrom(c)
	rows, total, err := h.svc.Audit.List(p)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.List(c, rows, metaFor(p, total))
}
