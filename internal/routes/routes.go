// Package routes wires every HTTP route, applying global middleware and
// role-based authorization per route group.
package routes

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/auth"
	"the-fulfillment/backend/internal/config"
	"the-fulfillment/backend/internal/docs"
	"the-fulfillment/backend/internal/handlers"
	"the-fulfillment/backend/internal/middleware"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/response"
)

// resetEnabled blocks the destructive data-reset route unless ALLOW_DATA_RESET
// is on and we're not in production — a safety valve against accidental wipes.
func resetEnabled(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !cfg.AllowDataReset || cfg.IsProduction() {
			response.AbortForbidden(c, "Chức năng xoá dữ liệu đang tắt trên môi trường này")
			return
		}
		c.Next()
	}
}

// Role sets reused across route groups.
var (
	roleAdminOwner = []models.Role{models.RoleOwner, models.RoleAdmin}
	roleOpsAdmin   = []models.Role{models.RoleOwner, models.RoleAdmin, models.RoleOps}
	roleDesignOps  = []models.Role{models.RoleOwner, models.RoleAdmin, models.RoleOps, models.RoleDesigner}
	roleProdOps    = []models.Role{models.RoleOwner, models.RoleAdmin, models.RoleOps, models.RoleProduction, models.RoleDesigner}
	roleQCOps      = []models.Role{models.RoleOwner, models.RoleAdmin, models.RoleOps, models.RoleQC}
	rolePackOps    = []models.Role{models.RoleOwner, models.RoleAdmin, models.RoleOps, models.RolePacking}
	roleShipOps    = []models.Role{models.RoleOwner, models.RoleAdmin, models.RoleOps, models.RolePacking, models.RoleShipping}
	// Every internal (non-seller) role — for read-only operational screens.
	roleInternal = []models.Role{
		models.RoleOwner, models.RoleAdmin, models.RoleOps, models.RoleDesigner,
		models.RoleProduction, models.RoleQC, models.RolePacking, models.RoleShipping,
	}
)

// New builds the configured Gin engine.
func New(cfg *config.Config, h *handlers.Handlers, jwt *auth.Manager) *gin.Engine {
	if cfg.IsProduction() {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.New()
	r.Use(middleware.Recovery())
	r.Use(middleware.RequestLogger())
	r.Use(middleware.CORS(cfg.CORSAllowedOrigins))

	// Health + docs (public).
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"status": "ok", "service": cfg.AppName}})
	})
	r.GET("/openapi.yaml", docs.Spec)
	r.GET("/docs", docs.UI)

	api := r.Group("/api")

	// Public auth.
	api.POST("/auth/login", h.Login)

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

	// Admin / danger zone (OWNER only, and only when ALLOW_DATA_RESET is enabled).
	// POST /api/admin/reset wipes order/production data so the catalog can be
	// re-imported from scratch; master data and users are preserved.
	admin := authd.Group("/admin", middleware.RequireRoles(models.RoleOwner))
	{
		admin.POST("/reset", resetEnabled(cfg), h.ResetData)
	}

	// Sellers (ops/admin/owner write; internal read).
	sellers := authd.Group("/sellers")
	{
		sellers.GET("", middleware.RequireRoles(roleInternal...), h.ListSellers)
		sellers.GET("/:id", middleware.RequireRoles(roleInternal...), h.GetSeller)
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

	// Materials (ops/admin/owner write; internal read).
	materials := authd.Group("/materials")
	{
		materials.GET("", middleware.RequireRoles(roleInternal...), h.ListMaterials)
		materials.GET("/:id", middleware.RequireRoles(roleInternal...), h.GetMaterial)
		materials.POST("", middleware.RequireRoles(roleOpsAdmin...), h.CreateMaterial)
		materials.PUT("/:id", middleware.RequireRoles(roleOpsAdmin...), h.UpdateMaterial)
		materials.DELETE("/:id", middleware.RequireRoles(roleAdminOwner...), h.DeleteMaterial)
	}

	// SKUs.
	skus := authd.Group("/skus")
	{
		skus.GET("", middleware.RequireRoles(roleInternal...), h.ListSKUs)
		skus.GET("/:id", middleware.RequireRoles(roleInternal...), h.GetSKU)
		skus.POST("", middleware.RequireRoles(roleOpsAdmin...), h.CreateSKU)
		skus.PUT("/:id", middleware.RequireRoles(roleOpsAdmin...), h.UpdateSKU)
		skus.DELETE("/:id", middleware.RequireRoles(roleAdminOwner...), h.DeleteSKU)
	}

	// Orders + import (internal).
	orders := authd.Group("/orders")
	{
		orders.GET("", middleware.RequireRoles(roleInternal...), h.ListOrders)
		orders.GET("/:id", middleware.RequireRoles(roleInternal...), h.GetOrder)
		orders.POST("", middleware.RequireRoles(roleOpsAdmin...), h.CreateOrderDirect)
		orders.GET("/import/template.xlsx", middleware.RequireRoles(roleOpsAdmin...), h.DownloadOrderImportTemplate)
		orders.POST("/import", middleware.RequireRoles(roleOpsAdmin...), h.ImportOrders)
		orders.POST("/import/commit", middleware.RequireRoles(roleOpsAdmin...), h.CommitImport)
	}
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
		review.GET("/:id", h.GetReviewOrder)
		review.POST("/:id/approve", h.ApproveReviewOrder)
		review.POST("/:id/reject", h.RejectReviewOrder)
		review.POST("/:id/request-correction", h.RequestReviewCorrection)
	}

	// Cancellation requests (ops/admin resolve seller-submitted requests).
	cancellations := authd.Group("/cancellation-requests", middleware.RequireRoles(roleOpsAdmin...))
	{
		cancellations.GET("", h.ListCancellationRequests)
		cancellations.POST("/:id/approve", h.ApproveCancellation)
		cancellations.POST("/:id/reject", h.RejectCancellation)
	}

	// Items + design queue.
	items := authd.Group("/items")
	{
		items.GET("", middleware.RequireRoles(roleInternal...), h.ListItems)
		items.GET("/:id", middleware.RequireRoles(roleInternal...), h.GetItem)
		items.PATCH("/:id/design", middleware.RequireRoles(roleDesignOps...), h.UpdateItemDesign)
	}
	design := authd.Group("/design-queue", middleware.RequireRoles(roleDesignOps...))
	{
		design.GET("", h.DesignQueue)
		design.GET("/material-buckets", h.MaterialBuckets)
		design.GET("/material/:materialId/items", h.DesignReadyItemsForMaterial)
	}

	// Batches (designer creates; production drives status).
	batches := authd.Group("/batches")
	{
		batches.GET("", middleware.RequireRoles(roleInternal...), h.ListBatches)
		batches.GET("/:id", middleware.RequireRoles(roleInternal...), h.GetBatch)
		batches.GET("/:id/production-template.xlsx", middleware.RequireRoles(roleInternal...), h.ExportProductionTemplate)
		batches.POST("", middleware.RequireRoles(roleDesignOps...), h.CreateBatch)
		batches.PATCH("/:id/status", middleware.RequireRoles(roleProdOps...), h.UpdateBatchStatus)
	}

	// QC.
	qc := authd.Group("/qc", middleware.RequireRoles(roleQCOps...))
	{
		qc.POST("/scan", h.QCScan)
		qc.POST("/pass", h.QCPass)
		qc.POST("/fail", h.QCFail)
	}

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
	}

	// Notes / required attention (all internal roles).
	notes := authd.Group("/notes", middleware.RequireRoles(roleInternal...))
	{
		notes.POST("", h.CreateNote)
		notes.GET("", h.ListNotes)
		notes.GET("/:id", h.GetNote)
		notes.PUT("/:id", h.UpdateNote)
		notes.DELETE("/:id", h.DeleteNote)
	}

	// Seller view (seller only — high-level status, no internal detail).
	seller := authd.Group("/seller", middleware.RequireRoles(models.RoleSeller))
	{
		seller.GET("/orders", h.SellerOrders)
		seller.GET("/orders/:id", h.SellerOrderDetail)
		// Seller self-upload: seller_id is forced to the authenticated seller.
		seller.GET("/orders/import/template.xlsx", h.DownloadOrderImportTemplate)
		seller.POST("/orders/import", h.SellerImportOrders)
		seller.POST("/orders/import/commit", h.SellerCommitImport)
		seller.POST("/orders/:id/cancel", h.SellerCancelOrder)
		seller.POST("/orders/:id/cancellation-request", h.SellerRequestCancellation)
	}

	return r
}
