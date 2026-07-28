package handlers

import (
	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/apperr"
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

// BulkDeleteNotes removes many notes in one request, in one of two modes:
//
//	{ "ids": [1,2,3] }                       — exactly these notes
//	{ "all": true, "status": "OPEN", … }     — EVERY note matching the filter
//
// The second mode is what the screen's "chọn tất cả" uses. It sends the filter
// the user is looking at rather than a list of ids, because the match can be tens
// of thousands of rows: naming them all would mean a huge request and, worse, a
// silent cap at whatever page the client had loaded — the user would tick "all"
// and get only the current page deleted.
// POST /api/notes/bulk-delete
func (h *Handlers) BulkDeleteNotes(c *gin.Context) {
	var in struct {
		IDs []uint `json:"ids"`
		All bool   `json:"all"`
		// Filter, only read when All is set. Mirrors the list endpoint's query.
		Status            string `json:"status"`
		Severity          string `json:"severity"`
		EntityType        string `json:"entity_type"`
		EntityID          *uint  `json:"entity_id"`
		RequiredAttention *bool  `json:"required_attention"`
	}
	if !bindJSON(c, &in) {
		return
	}

	if in.All {
		f := repositories.NoteFilter{
			Status:            in.Status,
			Severity:          in.Severity,
			EntityType:        in.EntityType,
			EntityID:          in.EntityID,
			RequiredAttention: in.RequiredAttention,
		}
		n, err := h.svc.Note.DeleteNotesMatching(actor(c), f)
		if err != nil {
			response.Fail(c, err)
			return
		}
		// Same shape as the id path so the client reads one field either way.
		response.OK(c, gin.H{"deleted_count": n, "deleted_ids": []uint{}, "missing_ids": []uint{}})
		return
	}

	if len(in.IDs) == 0 {
		response.Fail(c, apperr.BadRequest("Chưa chọn ghi chú nào để xoá"))
		return
	}
	res, err := h.svc.Note.DeleteNotes(actor(c), in.IDs)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{
		"deleted_count": len(res.DeletedIDs),
		"deleted_ids":   res.DeletedIDs,
		"missing_ids":   res.MissingIDs,
	})
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
