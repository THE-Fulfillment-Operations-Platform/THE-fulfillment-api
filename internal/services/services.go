// Package services holds the business logic. Handlers call services; services
// call repositories. No SQL or HTTP types leak across these boundaries.
package services

import (
	"the-fulfillment/backend/internal/auth"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
	"the-fulfillment/backend/internal/shipping"
	"the-fulfillment/backend/internal/tracking24h"
)

// Actor is the authenticated user performing an action. It is threaded through
// the service layer for created_by stamps and audit logging.
type Actor struct {
	ID       uint
	Email    string
	Role     models.Role
	SellerID *uint
	IP       string
	// Perms is the caller's effective permission set, filled from the request's
	// access (see models.Access). nil means "the role's defaults" — what tests
	// and internal callers that only know a role get.
	Perms map[string]bool
}

// Can reports whether the actor holds perm (e.g. models.Manage(models.FeatOrders)).
// OWNER holds everything.
func (a Actor) Can(perm string) bool {
	if a.Role == models.RoleOwner {
		return true
	}
	if a.Perms != nil {
		return a.Perms[perm]
	}
	for _, p := range models.RoleDefaultPerms(a.Role) {
		if p == perm {
			return true
		}
	}
	return false
}

// IDPtr returns a pointer to the actor id, or nil for an anonymous actor.
func (a Actor) IDPtr() *uint {
	if a.ID == 0 {
		return nil
	}
	id := a.ID
	return &id
}

// Services bundles every service for easy injection into handlers.
type Services struct {
	Auth   *AuthService
	User   *UserService
	Access *AccessService
	Seller *SellerService
	// APIKey issues the sellers' open-API keys and resolves the key a request
	// presents; Open is the open API itself (/api/open/v1).
	APIKey       *APIKeyService
	Open         *OpenAPIService
	Catalog      *CatalogService
	Import       *ImportService
	MasterImport *MasterImportService
	Order        *OrderService
	Review       *ReviewService
	Batch        *BatchService
	QC           *QCService
	Packing      *PackingService
	Note         *NoteService
	Audit        *AuditService
	Admin        *AdminService
	// TrackingSync is always present; it reports Enabled()=false when no provider
	// is configured, so callers never branch on nil.
	TrackingSync *TrackingSyncService
	// Thumb is always present too; it reports Enabled()=false when no cache
	// directory is configured, and then simply serves nothing.
	Thumb *ThumbService
	// Carrier is the THE integration (connection settings, shipments, labels).
	// Always present; it does nothing until an owner turns it on in Settings.
	Carrier *CarrierService
}

// TrackingOptions configures the 24hTrack shipment-tracking integration. A nil
// Client leaves the integration off and tracking stays a manual field.
type TrackingOptions struct {
	Client  *tracking24h.Client
	Tag     string
	Resolve bool
}

// New builds the service bundle. appBaseURL is the public web-app origin used
// for human-clickable links in exported files.
func New(repo *repositories.Repositories, jwt *auth.Manager, carrier shipping.Carrier, track TrackingOptions, thumbs ThumbOptions, appBaseURL string, carrierOpts ...CarrierOptions) *Services {
	audit := NewAuditService(repo)
	trackingSync := NewTrackingSyncService(repo, audit, track.Client, track.Tag, track.Resolve)
	var copts CarrierOptions
	if len(carrierOpts) > 0 {
		copts = carrierOpts[0]
	}
	theSvc := &CarrierService{repo: repo, audit: audit, tracking: trackingSync, opts: copts}
	thumb := NewThumbService(repo, thumbs)
	access := NewAccessService(repo)
	imports := &ImportService{repo: repo, audit: audit}
	return &Services{
		Auth:         &AuthService{repo: repo, jwt: jwt, audit: audit},
		User:         &UserService{repo: repo, audit: audit, access: access},
		Access:       access,
		Seller:       &SellerService{repo: repo, audit: audit},
		APIKey:       NewAPIKeyService(repo, audit),
		Open:         &OpenAPIService{repo: repo, audit: audit, imports: imports},
		Catalog:      &CatalogService{repo: repo, audit: audit},
		Import:       imports,
		MasterImport: &MasterImportService{repo: repo, audit: audit},
		Order:        &OrderService{repo: repo, audit: audit, tracking: trackingSync},
		Review:       &ReviewService{repo: repo, audit: audit},
		Batch:        &BatchService{repo: repo, audit: audit, appBaseURL: appBaseURL},
		QC:           &QCService{repo: repo, audit: audit, thumb: thumb},
		Packing:      &PackingService{repo: repo, audit: audit, carrier: carrier, tracking: trackingSync, the: theSvc},
		Note:         &NoteService{repo: repo, audit: audit},
		Audit:        audit,
		Admin:        &AdminService{repo: repo, audit: audit},
		TrackingSync: trackingSync,
		Thumb:        thumb,
		Carrier:      theSvc,
	}
}
