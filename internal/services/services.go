// Package services holds the business logic. Handlers call services; services
// call repositories. No SQL or HTTP types leak across these boundaries.
package services

import (
	"the-fulfillment/backend/internal/auth"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
	"the-fulfillment/backend/internal/shipping"
)

// Actor is the authenticated user performing an action. It is threaded through
// the service layer for created_by stamps and audit logging.
type Actor struct {
	ID       uint
	Email    string
	Role     models.Role
	SellerID *uint
	IP       string
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
	Auth         *AuthService
	User         *UserService
	Seller       *SellerService
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
}

// New builds the service bundle.
func New(repo *repositories.Repositories, jwt *auth.Manager, carrier shipping.Carrier) *Services {
	audit := &AuditService{repo: repo}
	return &Services{
		Auth:         &AuthService{repo: repo, jwt: jwt, audit: audit},
		User:         &UserService{repo: repo, audit: audit},
		Seller:       &SellerService{repo: repo, audit: audit},
		Catalog:      &CatalogService{repo: repo, audit: audit},
		Import:       &ImportService{repo: repo, audit: audit},
		MasterImport: &MasterImportService{repo: repo, audit: audit},
		Order:        &OrderService{repo: repo, audit: audit},
		Review:       &ReviewService{repo: repo, audit: audit},
		Batch:        &BatchService{repo: repo, audit: audit},
		QC:           &QCService{repo: repo, audit: audit},
		Packing:      &PackingService{repo: repo, audit: audit, carrier: carrier},
		Note:         &NoteService{repo: repo, audit: audit},
		Audit:        audit,
		Admin:        &AdminService{repo: repo, audit: audit},
	}
}
