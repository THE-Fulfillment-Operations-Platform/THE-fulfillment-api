// Package routes wires every HTTP route, applying global middleware and
// role-based authorization per route group.
package routes

import (
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/auth"
	"the-fulfillment/backend/internal/config"
	"the-fulfillment/backend/internal/docs"
	"the-fulfillment/backend/internal/handlers"
	"the-fulfillment/backend/internal/middleware"
	"the-fulfillment/backend/internal/models"
)

// Role sets reused across route groups.
var (
	roleAdminOwner = []models.Role{models.RoleOwner, models.RoleAdmin}
	roleOpsAdmin   = []models.Role{models.RoleOwner, models.RoleAdmin, models.RoleOps}
	roleDesignOps  = []models.Role{models.RoleOwner, models.RoleAdmin, models.RoleOps, models.RoleDesigner}
	roleProdOps    = []models.Role{models.RoleOwner, models.RoleAdmin, models.RoleOps, models.RoleProduction, models.RoleDesigner}
	roleQCOps      = []models.Role{models.RoleOwner, models.RoleAdmin, models.RoleOps, models.RoleQC}
	rolePackOps    = []models.Role{models.RoleOwner, models.RoleAdmin, models.RoleOps, models.RolePacking}
	// Huỷ batch (cả tấm hỏng, vứt đi, làm lại) là việc của người đứng cạnh cái
	// tấm đó: xưởng sản xuất phát hiện cắt/in hỏng, QC phát hiện cả tấm sai file.
	// DESIGNER cố tình không có ở đây — họ gom batch và xoá batch chưa sản xuất,
	// còn ghi bỏ vật liệu đã tiêu thì không.
	roleScrapBatch = []models.Role{
		models.RoleOwner, models.RoleAdmin, models.RoleOps,
		models.RoleProduction, models.RoleQC,
	}
	// Roles that may hand finished goods to THE — mirrors canShipToCarrier in the
	// service. SHIPPING belongs here (it is literally their desk); CS does not.
	roleShipCarrier = []models.Role{
		models.RoleOwner, models.RoleAdmin, models.RoleOps,
		models.RolePacking, models.RoleShipping,
	}
	// Roles that may record a tracking number. CS is here because attaching the
	// carrier's tracking number to a store order IS their job — they are the ones
	// who receive it, and the shipping desk only sees parcels it dispatched itself.
	roleShipOps = []models.Role{
		models.RoleOwner, models.RoleAdmin, models.RoleOps,
		models.RolePacking, models.RoleShipping, models.RoleCS,
	}
	// Every internal (non-seller) role — for read-only operational screens.
	// CS is deliberately NOT in here: customer support has no business on the
	// production board, the design queue or the batch screens. They get the
	// order-facing routes below instead.
	roleInternal = []models.Role{
		models.RoleOwner, models.RoleAdmin, models.RoleOps, models.RoleDesigner,
		models.RoleProduction, models.RoleQC, models.RolePacking, models.RoleShipping,
	}
	// Order-facing reads: everything roleInternal may see, plus CS. This is the
	// customer-support surface — look an order up, read who it ships to, follow
	// its parcel — and nothing else.
	roleOrderRead = append(append([]models.Role{}, roleInternal...), models.RoleCS)
)

// New builds the configured Gin engine.
func New(cfg *config.Config, h *handlers.Handlers, jwt *auth.Manager) *gin.Engine {
	if cfg.IsProduction() {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.New()
	// Control which proxies are trusted for client-IP derivation. By default gin
	// trusts ALL proxies, so a spoofed X-Forwarded-For would set ClientIP() and
	// let an attacker bypass per-IP rate limiting. Trust only configured proxies
	// (empty ⇒ none ⇒ ClientIP() = real TCP peer).
	if err := r.SetTrustedProxies(cfg.TrustedProxies); err != nil {
		log.Printf("routes: SetTrustedProxies failed: %v", err)
	}
	r.Use(middleware.Recovery())
	r.Use(middleware.RequestID())
	r.Use(middleware.RequestLogger())
	r.Use(middleware.CORS(cfg.CORSAllowedOrigins))
	r.Use(middleware.BodyLimit(cfg.MaxBodyBytes))

	// Health + docs (public). HEAD is registered too: uptime monitors and load
	// balancers commonly probe with HEAD, which would otherwise 404.
	health := func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"status": "ok", "service": cfg.AppName}})
	}
	r.GET("/health", health)
	r.HEAD("/health", health)
	r.GET("/openapi.yaml", docs.Spec)
	r.GET("/docs", docs.UI)

	api := r.Group("/api")

	// Public auth. Rate-limited per IP to blunt brute-force / credential stuffing
	// (10 attempts/minute); successful logins are well under that.
	api.POST("/auth/login", middleware.RateLimit(10, time.Minute), h.Login)

	// Cached mockup thumbnails for the QC station. Public by necessity — an <img>
	// tag cannot send the bearer token — so the URL carries its own HMAC
	// signature, item scope and expiry, minted by the QC scan response. See
	// handlers.ThumbnailAsset.
	api.GET("/assets/thumb/:name", h.ThumbnailAsset)

	// Authenticated routes.
	authd := api.Group("")
	authd.Use(middleware.Auth(jwt))

	authd.GET("/me", h.Me)

	// Users (admin/owner).
	users := authd.Group("/users", middleware.RequireRoles(roleAdminOwner...))
	{
		users.POST("", h.CreateUser)
		users.GET("", h.ListUsers)
		users.GET("/:id", h.GetUser)
		users.PUT("/:id", h.UpdateUser)
		users.DELETE("/:id", h.DeleteUser)
	}

	// Audit logs (admin/owner).
	authd.GET("/audit-logs", middleware.RequireRoles(roleAdminOwner...), h.ListAuditLogs)

	// Admin / danger zone (OWNER only). POST /api/admin/reset wipes
	// order/production data so the catalog can be re-imported from scratch;
	// master data and users are preserved. The OWNER role is the whole gate:
	// anyone holding it may run the reset, in any environment.
	admin := authd.Group("/admin", middleware.RequireRoles(models.RoleOwner))
	{
		admin.POST("/reset", h.ResetData)
	}

	// Sellers (ops/admin/owner write; internal read).
	sellers := authd.Group("/sellers")
	{
		sellers.GET("", middleware.RequireRoles(roleOrderRead...), h.ListSellers)
		sellers.GET("/:id", middleware.RequireRoles(roleOrderRead...), h.GetSeller)
		sellers.POST("", middleware.RequireRoles(roleOpsAdmin...), h.CreateSeller)
		sellers.PUT("/:id", middleware.RequireRoles(roleOpsAdmin...), h.UpdateSeller)
		sellers.DELETE("/:id", middleware.RequireRoles(roleAdminOwner...), h.DeleteSeller)
	}

	// Stores.
	stores := authd.Group("/stores")
	{
		stores.GET("", middleware.RequireRoles(roleInternal...), h.ListStores)
		stores.GET("/:id", middleware.RequireRoles(roleInternal...), h.GetStore)
		stores.POST("", middleware.RequireRoles(roleOpsAdmin...), h.CreateStore)
		stores.PUT("/:id", middleware.RequireRoles(roleOpsAdmin...), h.UpdateStore)
		stores.DELETE("/:id", middleware.RequireRoles(roleAdminOwner...), h.DeleteStore)
	}

	// Materials (ops/admin/owner write; internal read). Quota import is OWNER-only
	// because it sets the production quota, an OWNER-only lever.
	materials := authd.Group("/materials")
	{
		materials.GET("", middleware.RequireRoles(roleInternal...), h.ListMaterials)
		materials.GET("/import/template.xlsx", middleware.RequireRoles(models.RoleOwner), h.DownloadMaterialTemplate)
		materials.POST("/import/preview", middleware.RequireRoles(models.RoleOwner), h.MaterialImportPreview)
		materials.POST("/import/commit", middleware.RequireRoles(models.RoleOwner), h.MaterialImportCommit)
		materials.GET("/:id", middleware.RequireRoles(roleInternal...), h.GetMaterial)
		materials.POST("", middleware.RequireRoles(roleOpsAdmin...), h.CreateMaterial)
		materials.PUT("/:id", middleware.RequireRoles(roleOpsAdmin...), h.UpdateMaterial)
		materials.DELETE("/:id", middleware.RequireRoles(roleAdminOwner...), h.DeleteMaterial)
		materials.POST("/bulk-delete", middleware.RequireRoles(roleAdminOwner...), h.BulkDeleteMaterials)
	}

	// SKUs.
	skus := authd.Group("/skus")
	{
		skus.GET("", middleware.RequireRoles(roleInternal...), h.ListSKUs)
		skus.GET("/:id", middleware.RequireRoles(roleInternal...), h.GetSKU)
		skus.POST("", middleware.RequireRoles(roleOpsAdmin...), h.CreateSKU)
		skus.PUT("/:id", middleware.RequireRoles(roleOpsAdmin...), h.UpdateSKU)
		skus.DELETE("/:id", middleware.RequireRoles(roleAdminOwner...), h.DeleteSKU)
		skus.POST("/bulk-delete", middleware.RequireRoles(roleAdminOwner...), h.BulkDeleteSKUs)
		skus.POST("/bulk-active", middleware.RequireRoles(roleOpsAdmin...), h.BulkSetSKUsActive)
	}

	// Orders + import (internal).
	orders := authd.Group("/orders")
	{
		orders.GET("", middleware.RequireRoles(roleOrderRead...), h.ListOrders)
		orders.GET("/:id", middleware.RequireRoles(roleOrderRead...), h.GetOrder)
		orders.POST("", middleware.RequireRoles(roleOpsAdmin...), h.CreateOrderDirect)
		orders.GET("/import/template.xlsx", middleware.RequireRoles(roleOpsAdmin...), h.DownloadOrderImportTemplate)
		orders.POST("/import", middleware.RequireRoles(roleOpsAdmin...), h.ImportOrders)
		orders.POST("/import/commit", middleware.RequireRoles(roleOpsAdmin...), h.CommitImport)
		// Edit / cancel / delete — authorization is also re-checked in the service.
		orders.PUT("/:id", middleware.RequireRoles(roleOpsAdmin...), h.UpdateOrder)
		orders.POST("/:id/cancel", middleware.RequireRoles(roleOpsAdmin...), h.CancelOrder)
		orders.DELETE("/:id", middleware.RequireRoles(roleAdminOwner...), h.DeleteOrder)
		// Send QC-finished orders to THE. This is the step that ends the factory
		// flow and starts the shipping one; the packing-scan route below still
		// exists for stations that use it. ship-scan is the station flow (scan one
		// parcel, it ships); ship-to-carrier is the bulk fallback by order ids.
		orders.POST("/ship-to-carrier", middleware.RequireRoles(roleShipCarrier...), h.ShipOrdersToCarrier)
		orders.POST("/ship-scan", middleware.RequireRoles(roleShipCarrier...), h.ShipScannedOrder)
		// Tracking: ops + the packing/shipping stations may set it.
		orders.PATCH("/:id/tracking", middleware.RequireRoles(roleShipOps...), h.UpdateOrderTracking)
		// Bulk tracking assignment from the CS Excel: template → preview (dry run,
		// nothing written) → commit (only the confirmed assignments). Same roles as
		// the single-order edit — this is the same act, many rows at once.
		orders.GET("/tracking/import/template.xlsx", middleware.RequireRoles(roleShipOps...), h.DownloadTrackingImportTemplate)
		orders.POST("/tracking/import", middleware.RequireRoles(roleShipOps...), h.PreviewTrackingImport)
		orders.POST("/tracking/import/commit", middleware.RequireRoles(roleShipOps...), h.CommitTrackingImport)
		// The shipment journey is read-only operational information — every
		// internal role that can open an order may see where its parcel is.
		orders.GET("/:id/tracking/events", middleware.RequireRoles(roleOrderRead...), h.GetOrderTracking)
		// Pulling from the provider costs quota and rate limit, so it stays with
		// the roles that own tracking.
		orders.POST("/:id/tracking/sync", middleware.RequireRoles(roleShipOps...), h.SyncOrderTracking)
	}

	// Run one provider pass by hand instead of waiting for the scheduler.
	authd.POST("/tracking/sync", middleware.RequireRoles(roleShipOps...), h.RunTrackingSync)
	authd.GET("/import-jobs", middleware.RequireRoles(roleOpsAdmin...), h.ListImportJobs)
	authd.GET("/import-jobs/:id", middleware.RequireRoles(roleOpsAdmin...), h.GetImportJob)

	// Master-data setup: import the factory's legacy operational spreadsheet to
	// seed Materials, SKUs and the SKU↔Material mapping (preview → commit).
	masterData := authd.Group("/master-data", middleware.RequireRoles(roleOpsAdmin...))
	{
		masterData.GET("/template.xlsx", h.DownloadMasterTemplate)
		masterData.POST("/import/preview", h.MasterImportPreview)
		masterData.POST("/import/commit", h.MasterImportCommit)
		masterData.GET("/import-jobs", h.ListMasterImportJobs)
		masterData.GET("/import-jobs/:id", h.GetMasterImportJob)
	}

	// Order review / intake (Pending Review). Ops/Designer approve orders before
	// they enter the design/production flow.
	review := authd.Group("/review/orders", middleware.RequireRoles(roleDesignOps...))
	{
		review.GET("", h.ListReviewOrders)
		review.POST("/bulk-approve", h.BulkApproveReviewOrders)
		review.GET("/:id", h.GetReviewOrder)
		review.POST("/:id/approve", h.ApproveReviewOrder)
		review.POST("/:id/reject", h.RejectReviewOrder)
		review.POST("/:id/request-correction", h.RequestReviewCorrection)
	}

	// Cancellation requests (ops/admin resolve seller-submitted requests).
	cancellations := authd.Group("/cancellation-requests", middleware.RequireRoles(roleOpsAdmin...))
	{
		cancellations.GET("", h.ListCancellationRequests)
		cancellations.GET("/resolved", h.ListResolvedCancellations)
		cancellations.POST("/:id/approve", h.ApproveCancellation)
		cancellations.POST("/:id/reject", h.RejectCancellation)
		cancellations.GET("/items", h.ListItemCancellationRequests)
		cancellations.POST("/items/:id/approve", h.ApproveItemCancellation)
		cancellations.POST("/items/:id/reject", h.RejectItemCancellation)
	}

	// Items + design queue.
	items := authd.Group("/items")
	{
		items.GET("", middleware.RequireRoles(roleInternal...), h.ListItems)
		items.GET("/:id", middleware.RequireRoles(roleInternal...), h.GetItem)
		items.PATCH("/:id/design", middleware.RequireRoles(roleDesignOps...), h.UpdateItemDesign)
	}
	// Sidebar badges — one request for all counters the caller's sidebar shows.
	// roleOrderRead, not roleInternal: CS carries the notes badge, and refusing
	// them here meant their sidebar poll 403'd every 30 seconds for the whole
	// session (silently — the badge treats any failure as "keep the old number").
	authd.GET("/action-counts", middleware.RequireRoles(roleOrderRead...), h.ActionCounts)

	design := authd.Group("/design-queue", middleware.RequireRoles(roleDesignOps...))
	{
		design.GET("", h.DesignQueue)
		design.POST("/set-ready", h.BulkSetDesignReady)
		design.GET("/materials", h.DesignQueueMaterials)
		design.GET("/skus", h.DesignQueueSKUs)
		design.GET("/downloadable", h.DesignDownloadableItems)
		design.GET("/assets.zip", h.DownloadDesignAssetsZip)
		design.GET("/material-buckets", h.MaterialBuckets)
		design.GET("/material/:materialId/items", h.DesignReadyItemsForMaterial)
	}

	// Batches (designer creates; production drives status).
	batches := authd.Group("/batches")
	{
		batches.GET("", middleware.RequireRoles(roleInternal...), h.ListBatches)
		batches.GET("/:id", middleware.RequireRoles(roleInternal...), h.GetBatch)
		batches.GET("/:id/production-template.xlsx", middleware.RequireRoles(roleInternal...), h.ExportProductionTemplate)
		batches.GET("/:id/assets.zip", middleware.RequireRoles(roleInternal...), h.DownloadBatchAssetsZip)
		batches.POST("", middleware.RequireRoles(roleDesignOps...), h.CreateBatch)
		// Tự gom cả pool design-ready thành batch theo NVL + định mức: hệ thống
		// tự chọn sản phẩm và tự sinh mã, người vận hành chỉ bấm một nút.
		batches.POST("/auto", middleware.RequireRoles(roleDesignOps...), h.AutoCreateBatches)
		// Bàn làm việc Excel của designer: xuất mỗi dòng một batch (Batch ID bất
		// biến + mã + link hiện tại), điền link in/cắt rồi upload lại theo cặp
		// preview → commit. Cùng nhóm quyền với sửa link thủ công.
		batches.GET("/links/export.xlsx", middleware.RequireRoles(roleDesignOps...), h.ExportBatchLinksXLSX)
		batches.POST("/links/import/preview", middleware.RequireRoles(roleDesignOps...), h.PreviewBatchLinkImport)
		batches.POST("/links/import/commit", middleware.RequireRoles(roleDesignOps...), h.CommitBatchLinkImport)
		batches.PATCH("/:id/links", middleware.RequireRoles(roleDesignOps...), h.SetBatchLink)
		// PUT thay cả CẶP link in+cắt nguyên tử (một transaction, fan-out cả hai).
		batches.PUT("/:id/links", middleware.RequireRoles(roleDesignOps...), h.SetBatchLinkPair)
		// Delete mirrors create's roles: the team that groups batches un-groups a
		// mistaken one. The service only ever deletes a batch production has not
		// touched, so this is "undo create", not data destruction.
		batches.DELETE("/:id", middleware.RequireRoles(roleDesignOps...), h.DeleteBatch)
		batches.PATCH("/:id/status", middleware.RequireRoles(roleProdOps...), h.UpdateBatchStatus)
		// Huỷ batch: ngược lại với xoá. Xoá là undo của lệnh gom (chưa ai đụng
		// vào, xoá sạch dấu vết); huỷ là ghi nhận một tấm đã in/cắt hỏng thật.
		batches.POST("/:id/scrap", middleware.RequireRoles(roleScrapBatch...), h.ScrapBatch)
	}

	// QC.
	qc := authd.Group("/qc", middleware.RequireRoles(roleQCOps...))
	{
		qc.POST("/scan", h.QCScan)
		qc.POST("/pass", h.QCPass)
		qc.POST("/fail", h.QCFail)
	}
	// Hạ QC (bấm nhầm) nằm NGOÀI nhóm /qc ở trên vì nó chặt hơn: chỉ OWNER/ADMIN.
	// Nó mở lại một cửa đã đóng, và ranh giới giữa "bấm nhầm" với "hàng hỏng
	// nhưng ngại làm thủ tục huỷ" là thứ phải có người chịu trách nhiệm.
	authd.POST("/qc/undo", middleware.RequireRoles(roleAdminOwner...), h.QCUndoPass)

	// Kết quả QC: đọc-only, mở cho mọi vai trò nội bộ — đóng gói/OPS cần biết đơn
	// nào đã QC đủ để lấy hàng, không chỉ tổ QC.
	authd.GET("/qc/results", middleware.RequireRoles(roleInternal...), h.QCResults)

	// Packing.
	packing := authd.Group("/packing", middleware.RequireRoles(rolePackOps...))
	{
		packing.POST("/scan", h.PackingScan)
		packing.GET("/order/:id", h.GetOrderPackage)
	}

	// Handoffs — creating a handoff is packing/shipping; listing is read-only and
	// available to every internal role (the dashboard shows a handoff KPI).
	handoffs := authd.Group("/handoffs")
	{
		handoffs.POST("", middleware.RequireRoles(roleShipOps...), h.CreateHandoff)
		handoffs.GET("", middleware.RequireRoles(roleInternal...), h.ListHandoffs)
		handoffs.POST("/:id/ship", middleware.RequireRoles(roleShipOps...), h.MarkHandoffShipped)
	}

	// Notes / required attention (all internal roles).
	notes := authd.Group("/notes", middleware.RequireRoles(roleOrderRead...))
	{
		notes.POST("", h.CreateNote)
		notes.GET("", h.ListNotes)
		notes.GET("/:id", h.GetNote)
		notes.PUT("/:id", h.UpdateNote)
		notes.DELETE("/:id", h.DeleteNote)
		notes.POST("/bulk-delete", h.BulkDeleteNotes)
	}

	// Seller view (seller only — high-level status, no internal detail).
	seller := authd.Group("/seller", middleware.RequireRoles(models.RoleSeller))
	{
		seller.GET("/orders", h.SellerOrders)
		seller.GET("/orders/:id", h.SellerOrderDetail)
		// Where is my parcel — the same journey ops sees, scoped to the seller's
		// own orders (ownership is enforced in the service).
		seller.GET("/orders/:id/tracking/events", h.GetSellerOrderTracking)
		// What happened to my order before it became a parcel — the order-level
		// status trail (review decision, production, hand-off, cancellation).
		seller.GET("/orders/:id/history", h.SellerOrderHistory)
		// Seller may edit their own order while it is still in review.
		seller.PUT("/orders/:id", h.SellerUpdateOrder)
		// Seller self-upload: seller_id is forced to the authenticated seller.
		seller.GET("/orders/import/template.xlsx", h.DownloadOrderImportTemplate)
		seller.POST("/orders/import", h.SellerImportOrders)
		seller.POST("/orders/import/commit", h.SellerCommitImport)
		seller.POST("/orders/:id/cancel", h.SellerCancelOrder)
		seller.POST("/orders/:id/cancellation-request", h.SellerRequestCancellation)
		seller.POST("/orders/:id/items/:item_id/cancel", h.SellerCancelOrderItem)
		seller.POST("/orders/:id/items/:item_id/cancellation-request", h.SellerRequestItemCancellation)
	}

	return r
}
