package services

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// Batch link pair actions. ASSIGN completes a batch's production package (no
// existing link disagrees), REPLACE overwrites at least one existing link (and
// therefore needs a reason), UNCHANGED means the submitted pair is exactly what
// the batch already has — a no-op that must not fabricate history.
const (
	BatchLinkActionAssign    = "ASSIGN"
	BatchLinkActionReplace   = "REPLACE"
	BatchLinkActionUnchanged = "UNCHANGED"
)

// maxBatchLinkURLLen mirrors the BatchLink.URL column size — a longer URL would
// be silently truncated by Postgres, leaving the batch pointing at a file that
// does not exist.
const maxBatchLinkURLLen = 500

// maxLinkReasonLen bounds the replacement reason (it lands in audit metadata).
const maxLinkReasonLen = 255

// normalizeProductionURL trims and validates one production-file URL.
func normalizeProductionURL(label, raw string) (string, error) {
	u := strings.TrimSpace(raw)
	if u == "" {
		return "", apperr.BadRequest(label + " không được để trống")
	}
	if !isValidHTTPURL(u) {
		return "", apperr.BadRequest(label + " không hợp lệ (phải là http hoặc https)")
	}
	if len(u) > maxBatchLinkURLLen {
		return "", apperr.BadRequest(fmt.Sprintf("%s quá dài (tối đa %d ký tự)", label, maxBatchLinkURLLen))
	}
	return u, nil
}

// batchLinkLockedGuard rejects link edits on batches whose production package
// must not change: parent batches hold no items, closed batches have nothing
// left to produce, and a batch past PENDING has physically started production —
// its files are history. A wrong file after that point goes through the
// scrap/rework flow, never through rewriting the links.
func batchLinkLockedGuard(b *models.Batch) error {
	if b.IsParent {
		return apperr.Unprocessable("Batch mẹ không chứa item sản xuất. Hãy cập nhật link trên từng batch con.")
	}
	if b.ClosedAt != nil {
		return apperr.Unprocessable("Batch " + b.Code + " đã đóng — không sửa link sản xuất nữa.")
	}
	if b.Status != models.StatusPending {
		return apperr.Unprocessable("Batch " + b.Code + " đã bắt đầu sản xuất — bộ file đã bị khóa. Nếu sản xuất sai, dùng luồng huỷ/làm lại thay vì thay file lịch sử.")
	}
	return nil
}

// classifyLinkPairAction decides what applying (newPrint, newCut) over the
// current pair means. Empty current values mean "no link yet".
func classifyLinkPairAction(prevPrint, prevCut, newPrint, newCut string) string {
	if prevPrint == newPrint && prevCut == newCut {
		return BatchLinkActionUnchanged
	}
	if (prevPrint != "" && prevPrint != newPrint) || (prevCut != "" && prevCut != newCut) {
		return BatchLinkActionReplace
	}
	return BatchLinkActionAssign
}

// linkPairApplied reports what applying a pair to one batch actually did.
type linkPairApplied struct {
	Action    string
	PrevPrint string
	PrevCut   string
}

// applyBatchLinkPairTx is the single place a (print, cut) pair lands on a
// batch: the lock guards, the reason policy for replacements, both upserts and
// both per-item fan-outs, all inside the caller's transaction — either the
// whole package lands or none of it does. Both the manual pair endpoint and the
// Excel import funnel through here, so the two paths can never drift apart.
// The caller must already hold the batch's row lock in txRepo's transaction.
func applyBatchLinkPairTx(txRepo *repositories.Repositories, actor Actor, batch *models.Batch, printURL, cutURL, reason string, now time.Time) (*linkPairApplied, error) {
	if err := batchLinkLockedGuard(batch); err != nil {
		return nil, err
	}
	// A batch whose every part was scrapped or cancelled has nothing left to
	// produce — a fresh package on it could only be a mistake.
	items, err := txRepo.Batch.ActiveBatchItemsForBatch(batch.ID)
	if err != nil {
		return nil, apperr.Internal("could not read batch items").Wrap(err)
	}
	if len(items) == 0 {
		return nil, apperr.Unprocessable("Batch " + batch.Code + " không còn sản phẩm hợp lệ để gắn file sản xuất.")
	}

	prev := map[models.BatchLinkKind]*models.BatchLink{}
	for _, kind := range []models.BatchLinkKind{models.BatchLinkPrint, models.BatchLinkCut} {
		link, err := txRepo.Batch.FindLink(batch.ID, kind)
		if err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, apperr.Internal("could not look up batch link").Wrap(err)
			}
			continue
		}
		prev[kind] = link
	}
	res := &linkPairApplied{}
	if l := prev[models.BatchLinkPrint]; l != nil {
		res.PrevPrint = l.URL
	}
	if l := prev[models.BatchLinkCut]; l != nil {
		res.PrevCut = l.URL
	}
	res.Action = classifyLinkPairAction(res.PrevPrint, res.PrevCut, printURL, cutURL)
	if res.Action == BatchLinkActionUnchanged {
		// Resubmitting the identical pair changes nothing: no restamp, no audit.
		return res, nil
	}
	if res.Action == BatchLinkActionReplace && strings.TrimSpace(reason) == "" {
		return nil, apperr.Unprocessable("Batch " + batch.Code + " đã có bộ file sản xuất — cần lý do khi thay thế link.")
	}

	// The pair is one submission, so both rows get the same author + timestamp.
	upsert := func(kind models.BatchLinkKind, url string) error {
		if existing := prev[kind]; existing != nil {
			existing.URL = url
			existing.UpdatedByID = actor.IDPtr()
			existing.LinkUpdatedAt = now
			return txRepo.Batch.UpdateLink(existing)
		}
		return txRepo.Batch.CreateLink(&models.BatchLink{
			BatchID: batch.ID, Kind: kind, URL: url,
			UpdatedByID: actor.IDPtr(), LinkUpdatedAt: now,
		})
	}
	if err := upsert(models.BatchLinkPrint, printURL); err != nil {
		return nil, apperr.Internal("could not save print link").Wrap(err)
	}
	if err := upsert(models.BatchLinkCut, cutURL); err != nil {
		return nil, apperr.Internal("could not save cut link").Wrap(err)
	}
	if _, err := txRepo.OrderItem.SetProductionFileForBatch(batch.ID, "print_file_url", printURL); err != nil {
		return nil, apperr.Internal("could not apply print link to items").Wrap(err)
	}
	if _, err := txRepo.OrderItem.SetProductionFileForBatch(batch.ID, "cut_file_url", cutURL); err != nil {
		return nil, apperr.Internal("could not apply cut link to items").Wrap(err)
	}
	return res, nil
}

// SetBatchLinkPairInput submits a batch's complete production package: both the
// print and the cut file, together. Reason is required when the submission
// replaces an existing link.
type SetBatchLinkPairInput struct {
	PrintURL string `json:"print_url" binding:"required"`
	CutURL   string `json:"cut_url" binding:"required"`
	Reason   string `json:"reason"`
}

// BatchLinkPairResult returns the batch's links after the submission plus what
// the submission actually was (ASSIGN / REPLACE / UNCHANGED).
type BatchLinkPairResult struct {
	Links  []models.BatchLink `json:"links"`
	Action string             `json:"action"`
}

// SetBatchLinkPair saves a batch's print + cut links as one atomic package and
// fans both out to the batch's live items. A batch is never left with a fresh
// print link and a stale cut link: any failure rolls the whole submission back.
// PUT /api/batches/:id/links
func (s *BatchService) SetBatchLinkPair(actor Actor, batchID uint, in SetBatchLinkPairInput) (*BatchLinkPairResult, error) {
	printURL, err := normalizeProductionURL("Link in", in.PrintURL)
	if err != nil {
		return nil, err
	}
	cutURL, err := normalizeProductionURL("Link cắt", in.CutURL)
	if err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(in.Reason)
	if len(reason) > maxLinkReasonLen {
		return nil, apperr.BadRequest(fmt.Sprintf("Lý do quá dài (tối đa %d ký tự)", maxLinkReasonLen))
	}

	// Fast guard outside the transaction for a friendly error; the same guard
	// runs again on the locked row inside — the screen may be stale.
	batch, err := s.Get(batchID)
	if err != nil {
		return nil, err
	}
	if err := batchLinkLockedGuard(batch); err != nil {
		return nil, err
	}

	now := time.Now()
	var applied *linkPairApplied
	if err := s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)
		locked, err := txRepo.Batch.FindLiteManyForUpdate([]uint{batchID})
		if err != nil {
			return apperr.Internal("could not lock batch").Wrap(err)
		}
		if len(locked) == 0 {
			return apperr.NotFound("Batch not found")
		}
		applied, err = applyBatchLinkPairTx(txRepo, actor, &locked[0], printURL, cutURL, reason, now)
		return err
	}); err != nil {
		return nil, err
	}

	if applied.Action != BatchLinkActionUnchanged {
		s.audit.Log(actor, "BATCH_PRODUCTION_PACKAGE_SET", "batch", &batchID,
			fmt.Sprintf("Nộp bộ file sản xuất cho batch %s (%s)", batch.Code, applied.Action),
			models.JSONMap{
				"batch_code":     batch.Code,
				"prev_print_url": applied.PrevPrint,
				"prev_cut_url":   applied.PrevCut,
				"new_print_url":  printURL,
				"new_cut_url":    cutURL,
				"action":         applied.Action,
				"reason":         reason,
			})
	}

	links, err := s.repo.Batch.LinksForBatch(batchID)
	if err != nil {
		return nil, apperr.Internal("could not reload batch links").Wrap(err)
	}
	if links == nil {
		links = []models.BatchLink{}
	}
	return &BatchLinkPairResult{Links: links, Action: applied.Action}, nil
}
