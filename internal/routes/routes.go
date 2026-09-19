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
	"the-fulfillment/backend/internal/services"
)

// Most internal routes are guarded by permission ticks (see models/permissions.go):
// perm(view(X)) / perm(manage(X)), where a route shared by several screens names
// each screen that may use it. Roles still gate what is not a tick: user admin,
// the OWNER-only levers, and destructive actions (role AND the screen's manage).
var (
	roleAdminOwner = []models.Role{models.RoleOwner, models.RoleAdmin}
	// Every internal (non-seller) account — for reference data every screen
	// reads, and the sidebar badges.
	roleOrderRead = []models.Role{
		models.RoleOwner, models.RoleAdmin, models.RoleOps, models.RoleDesigner,
		models.RoleProduction, models.RoleQC, models.RolePacking, models.RoleShipping,
		models.RoleCS,
	}
	perm   = middleware.RequirePerm
	view   = models.View
	manage = models.Manage
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

	// Authenticated routes. LoadAccess re-reads the caller's role and permission
	// ticks from the database (cached ~30s), so every guard below judges the
	// account as it is now, not as it was when the token was issued.
	authd := api.Group("")
	authd.Use(middleware.Auth(jwt))
	authd.Use(middleware.LoadAccess(h.AccessLoader(), services.ErrAccessRevoked))

	authd.GET("/me", h.Me)

	// Users (admin/owner). Deliberately role-gated, not a tick: whoever manages
	// users could otherwise tick the rest for themselves.
	users := authd.Group("/users", middleware.RequireRoles(roleAdminOwner...))
	{
		users.POST("", h.CreateUser)
		users.GET("", h.ListUsers)
		users.GET("/:id", h.GetUser)
		users.PUT("/:id", h.UpdateUser)
		users.DELETE("/:id", h.DeleteUser)
	}
	// The tickable screens + each role's default ticks, for the user form.
	authd.GET("/permission-catalog", middleware.RequireRoles(roleAdminOwner...), h.PermissionCatalog)

	// Audit logs.
	authd.GET("/audit-logs", perm(view(models.FeatAudit)), h.ListAuditLogs)

	// Admin / danger zone (OWNER only). POST /api/admin/reset wipes
	// order/production data so the catalog can be re-imported from scratch;
	// master data and users are preserved. The OWNER role is the whole gate:
	// anyone holding it may run the reset, in any environment.
	admin := authd.Group("/admin", middleware.RequireRoles(models.RoleOwner))
	{
		admin.POST("/reset", h.ResetData)
	}

	// Reference data (sellers, stores, materials, SKUs) is READ by nearly every
	// screen — filters, pickers, labels — so reading it only needs an internal
	// account. Writing it is Master Data's "manage"; deleting it additionally
	// stays with ADMIN/OWNER.
	masterManage := perm(manage(models.FeatMasterData))
	adminOwner := middleware.RequireRoles(roleAdminOwner...)
	internalRead := middleware.RequireRoles(roleOrderRead...)

	sellers := authd.Group("/sellers")
	{
		sellers.GET("", internalRead, h.ListSellers)
		sellers.GET("/:id", internalRead, h.GetSeller)
		sellers.POST("", masterManage, h.CreateSeller)
		sellers.PUT("/:id", masterManage, h.UpdateSeller)
		sellers.DELETE("/:id", adminOwner, masterManage, h.DeleteSeller)
	}

	stores := authd.Group("/stores")
	{
		stores.GET("", internalRead, h.ListStores)
		stores.GET("/:id", internalRead, h.GetStore)
		stores.POST("", masterManage, h.CreateStore)
		stores.PUT("/:id", masterManage, h.UpdateStore)
		stores.DELETE("/:id", adminOwner, masterManage, h.DeleteStore)
	}

	// The material import used to be OWNER-only because it set the production
	// quota, an OWNER-only lever. The quota is derived from sizes now (khách chốt
	// 2026-09-18) and the import only carries sheet sizes — master data like the
	// SKU import — so it takes Master Data's "manage" like every other write here.
	materials := authd.Group("/materials")
	{
		materials.GET("", internalRead, h.ListMaterials)
		materials.GET("/import/template.xlsx", masterManage, h.DownloadMaterialTemplate)
		materials.POST("/import/preview", masterManage, h.MaterialImportPreview)
		materials.POST("/import/commit", masterManage, h.MaterialImportCommit)
		materials.GET("/:id", internalRead, h.GetMaterial)
		materials.POST("", masterManage, h.CreateMaterial)
		materials.PUT("/:id", masterManage, h.UpdateMaterial)
		materials.DELETE("/:id", adminOwner, masterManage, h.DeleteMaterial)
		materials.POST("/bulk-delete", adminOwner, masterManage, h.BulkDeleteMaterials)
	}

	skus := authd.Group("/skus")
	{
		skus.GET("", internalRead, h.ListSKUs)
		skus.GET("/:id", internalRead, h.GetSKU)
		skus.POST("", masterManage, h.CreateSKU)
		skus.PUT("/:id", masterManage, h.UpdateSKU)
		skus.DELETE("/:id", adminOwner, masterManage, h.DeleteSKU)
		skus.POST("/bulk-delete", adminOwner, masterManage, h.BulkDeleteSKUs)
		skus.POST("/bulk-active", masterManage, h.BulkSetSKUsActive)
	}

	// Orders. The order list/detail feed several screens (orders, CS lookup,
	// journeys, the ship queue, the dashboard) — any of them may read.
	orderRead := perm(view(models.FeatOrders), view(models.FeatCS), view(models.FeatJourneys),
		view(models.FeatDashboard), view(models.FeatShipQueue))
	ordersManage := perm(manage(models.FeatOrders))
	importManage := perm(manage(models.FeatImport))
	shipManage := perm(manage(models.FeatShipQueue))
	// Tracking is edited from the journeys board and from CS lookup.
	trackingManage := perm(manage(models.FeatJourneys), manage(models.FeatCS))
	orders := authd.Group("/orders")
	{
		orders.GET("", orderRead, h.ListOrders)
		orders.GET("/:id", orderRead, h.GetOrder)
		orders.POST("", perm(manage(models.FeatImport), manage(models.FeatOrders)), h.CreateOrderDirect)
		orders.GET("/import/template.xlsx", importManage, h.DownloadOrderImportTemplate)
		orders.POST("/import", importManage, h.ImportOrders)
		orders.POST("/import/commit", importManage, h.CommitImport)
		// Edit / cancel / delete — authorization is also re-checked in the service.
		orders.PUT("/:id", ordersManage, h.UpdateOrder)
		orders.POST("/:id/cancel", ordersManage, h.CancelOrder)
		orders.DELETE("/:id", adminOwner, ordersManage, h.DeleteOrder)
		orders.POST("/bulk-delete", adminOwner, ordersManage, h.BulkDeleteOrders)
		// Send QC-finished orders to THE. This is the step that ends the factory
		// flow and starts the shipping one; the packing-scan route below still
		// exists for stations that use it. ship-scan is the station flow (scan one
		// parcel, it ships); ship-to-carrier is the bulk fallback by order ids.
		orders.POST("/ship-to-carrier", shipManage, h.ShipOrdersToCarrier)
		orders.POST("/ship-scan", shipManage, h.ShipScannedOrder)
		orders.PATCH("/:id/tracking", trackingManage, h.UpdateOrderTracking)
		// Bulk tracking assignment from the CS Excel: template → preview (dry run,
		// nothing written) → commit (only the confirmed assignments). Same guard as
		// the single-order edit — this is the same act, many rows at once.
		orders.GET("/tracking/import/template.xlsx", trackingManage, h.DownloadTrackingImportTemplate)
		orders.POST("/tracking/import", trackingManage, h.PreviewTrackingImport)
		orders.POST("/tracking/import/commit", trackingManage, h.CommitTrackingImport)
		// The shipment journey is read-only operational information — whoever
		// can open the order may see where its parcel is.
		orders.GET("/:id/tracking/events", orderRead, h.GetOrderTracking)
		// Pulling from the provider costs quota and rate limit, so it stays with
		// whoever owns tracking.
		orders.POST("/:id/tracking/sync", trackingManage, h.SyncOrderTracking)
	}

	// Run one provider pass by hand instead of waiting for the scheduler.
	authd.POST("/tracking/sync", trackingManage, h.RunTrackingSync)
	authd.GET("/import-jobs", perm(view(models.FeatImport)), h.ListImportJobs)
	authd.GET("/import-jobs/:id", perm(view(models.FeatImport)), h.GetImportJob)

	// Master-data setup: import the factory's legacy operational spreadsheet to
	// seed Materials, SKUs and the SKU↔Material mapping (preview → commit).
	masterData := authd.Group("/master-data", masterManage)
	{
		masterData.GET("/template.xlsx", h.DownloadMasterTemplate)
		masterData.POST("/import/preview", h.MasterImportPreview)
		masterData.POST("/import/commit", h.MasterImportCommit)
		masterData.GET("/import-jobs", h.ListMasterImportJobs)
		masterData.GET("/import-jobs/:id", h.GetMasterImportJob)
		// Step 1 of the parent → child SKU setup: the parents themselves. Step 2
		// is the import above, whose "SKU cha" column files children under them.
		masterData.GET("/parents/template.xlsx", h.DownloadParentSKUTemplate)
		masterData.POST("/parents/import/preview", h.ParentSKUImportPreview)
		masterData.POST("/parents/import/commit", h.ParentSKUImportCommit)
	}

	// Order review / intake (Pending Review): orders are approved here before
	// they enter the design/production flow.
	reviewView := perm(view(models.FeatReview))
	reviewManage := perm(manage(models.FeatReview))
	review := authd.Group("/review/orders")
	{
		review.GET("", reviewView, h.ListReviewOrders)
		review.POST("/bulk-approve", reviewManage, h.BulkApproveReviewOrders)
		review.GET("/:id", reviewView, h.GetReviewOrder)
		review.POST("/:id/approve", reviewManage, h.ApproveReviewOrder)
		review.POST("/:id/reject", reviewManage, h.RejectReviewOrder)
		review.POST("/:id/request-correction", reviewManage, h.RequestReviewCorrection)
	}

	// Cancellation requests (resolve seller-submitted requests).
	cancelView := perm(view(models.FeatCancellations))
	cancelManage := perm(manage(models.FeatCancellations))
	cancellations := authd.Group("/cancellation-requests")
	{
		cancellations.GET("", cancelView, h.ListCancellationRequests)
		cancellations.GET("/resolved", cancelView, h.ListResolvedCancellations)
		cancellations.POST("/:id/approve", cancelManage, h.ApproveCancellation)
		cancellations.POST("/:id/reject", cancelManage, h.RejectCancellation)
		cancellations.GET("/items", cancelView, h.ListItemCancellationRequests)
		cancellations.POST("/items/:id/approve", cancelManage, h.ApproveItemCancellation)
		cancellations.POST("/items/:id/reject", cancelManage, h.RejectItemCancellation)
	}

	// Items (the orders screen is item-level) + design edits, which the design
	// queue and the review detail both make.
	itemRead := perm(view(models.FeatOrders), view(models.FeatDashboard))
	items := authd.Group("/items")
	{
		items.GET("", itemRead, h.ListItems)
		items.GET("/:id", itemRead, h.GetItem)
		items.PATCH("/:id/design", perm(manage(models.FeatDesign), manage(models.FeatReview)), h.UpdateItemDesign)
	}
	// Sidebar badges — one request for all counters the caller's sidebar shows.
	// Open to every internal account: the counts are scoped by role inside, and
	// refusing here made a sidebar poll 403 every 30 seconds for the whole
	// session (silently — the badge treats any failure as "keep the old number").
	authd.GET("/action-counts", internalRead, h.ActionCounts)

	// Design queue. The material buckets also feed "tạo batch" on the batch
	// screen.
	designView := perm(view(models.FeatDesign))
	design := authd.Group("/design-queue")
	{
		design.GET("", designView, h.DesignQueue)
		design.POST("/set-ready", perm(manage(models.FeatDesign)), h.BulkSetDesignReady)
		design.GET("/materials", designView, h.DesignQueueMaterials)
		design.GET("/skus", designView, h.DesignQueueSKUs)
		design.GET("/downloadable", designView, h.DesignDownloadableItems)
		design.GET("/assets.zip", designView, h.DownloadDesignAssetsZip)
		design.GET("/material-buckets", perm(view(models.FeatDesign), manage(models.FeatBatches)), h.MaterialBuckets)
		design.GET("/material/:materialId/items", perm(view(models.FeatDesign), manage(models.FeatBatches)), h.DesignReadyItemsForMaterial)
	}

	// Batches: the batch screen creates them (and their print/cut links); the
	// production board drives their status; scrapping a ruined sheet is its own
	// tick, held by the floor and QC rather than whoever groups batches.
	batchRead := perm(view(models.FeatBatches), view(models.FeatProduction))
	batchManage := perm(manage(models.FeatBatches))
	batches := authd.Group("/batches")
	{
		batches.GET("", perm(view(models.FeatBatches), view(models.FeatProduction), view(models.FeatDashboard)), h.ListBatches)
		batches.GET("/:id", batchRead, h.GetBatch)
		batches.GET("/:id/production-template.xlsx", batchRead, h.ExportProductionTemplate)
		batches.GET("/:id/assets.zip", batchRead, h.DownloadBatchAssetsZip)
		batches.POST("", batchManage, h.CreateBatch)
		// Tự gom cả pool design-ready thành batch theo NVL + định mức: hệ thống
		// tự chọn sản phẩm và tự sinh mã, người vận hành chỉ bấm một nút.
		batches.POST("/auto", batchManage, h.AutoCreateBatches)
		// Bàn làm việc Excel của designer: xuất mỗi dòng một batch (Batch ID bất
		// biến + mã + link hiện tại), điền link in/cắt rồi upload lại theo cặp
		// preview → commit. Cùng quyền với sửa link thủ công.
		batches.GET("/links/export.xlsx", batchManage, h.ExportBatchLinksXLSX)
		batches.POST("/links/import/preview", batchManage, h.PreviewBatchLinkImport)
		batches.POST("/links/import/commit", batchManage, h.CommitBatchLinkImport)
		batches.PATCH("/:id/links", batchManage, h.SetBatchLink)
		// PUT thay cả CẶP link in+cắt nguyên tử (một transaction, fan-out cả hai).
		batches.PUT("/:id/links", batchManage, h.SetBatchLinkPair)
		// Delete mirrors create: the team that groups batches un-groups a
		// mistaken one. The service only ever deletes a batch production has not
		// touched, so this is "undo create", not data destruction.
		batches.DELETE("/:id", batchManage, h.DeleteBatch)
		batches.PATCH("/:id/status", perm(manage(models.FeatProduction)), h.UpdateBatchStatus)
		// Huỷ batch: ngược lại với xoá. Xoá là undo của lệnh gom (chưa ai đụng
		// vào, xoá sạch dấu vết); huỷ là ghi nhận một tấm đã in/cắt hỏng thật.
		batches.POST("/:id/scrap", perm(manage(models.FeatBatchScrap)), h.ScrapBatch)
	}

	// QC. "Xem" is scanning to look an item up; "Thao tác" is the verdict.
	qc := authd.Group("/qc")
	{
		qc.POST("/scan", perm(view(models.FeatQC)), h.QCScan)
		qc.POST("/pass", perm(manage(models.FeatQC)), h.QCPass)
		qc.POST("/fail", perm(manage(models.FeatQC)), h.QCFail)
		// Hạ QC (bấm nhầm) chặt hơn: cần thêm vai trò OWNER/ADMIN. Nó mở lại một
		// cửa đã đóng, và ranh giới giữa "bấm nhầm" với "hàng hỏng nhưng ngại làm
		// thủ tục huỷ" là thứ phải có người chịu trách nhiệm.
		qc.POST("/undo", adminOwner, perm(manage(models.FeatQC)), h.QCUndoPass)
		// Kết quả QC: đọc-only; màn Chờ gửi hàng cũng dựa vào nó để biết đơn nào
		// đã QC đủ để lấy hàng.
		qc.GET("/results", perm(view(models.FeatQCResults), view(models.FeatShipQueue)), h.QCResults)
	}

	// Packing (the older station flow, still reachable off-menu) belongs to
	// the ship queue.
	packing := authd.Group("/packing")
	{
		packing.POST("/scan", shipManage, h.PackingScan)
		packing.GET("/order/:id", perm(view(models.FeatShipQueue)), h.GetOrderPackage)
	}

	// Handoffs — creating one is the ship queue's / journeys board's job;
	// listing is read-only (the dashboard shows a handoff KPI).
	handoffs := authd.Group("/handoffs")
	{
		handoffManage := perm(manage(models.FeatShipQueue), manage(models.FeatJourneys))
		handoffs.POST("", handoffManage, h.CreateHandoff)
		handoffs.GET("", perm(view(models.FeatShipQueue), view(models.FeatJourneys), view(models.FeatDashboard)), h.ListHandoffs)
		handoffs.POST("/:id/ship", handoffManage, h.MarkHandoffShipped)
	}

	// Notes / required attention.
	notesManage := perm(manage(models.FeatNotes))
	notes := authd.Group("/notes")
	{
		notes.POST("", notesManage, h.CreateNote)
		notes.GET("", perm(view(models.FeatNotes), view(models.FeatDashboard)), h.ListNotes)
		notes.GET("/:id", perm(view(models.FeatNotes)), h.GetNote)
		notes.PUT("/:id", notesManage, h.UpdateNote)
		notes.DELETE("/:id", notesManage, h.DeleteNote)
		notes.POST("/bulk-delete", notesManage, h.BulkDeleteNotes)
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
