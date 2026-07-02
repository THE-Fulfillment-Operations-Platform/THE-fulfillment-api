package services

import (
	"errors"
	"strings"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// SellerService manages sellers and their stores.
type SellerService struct {
	repo  *repositories.Repositories
	audit *AuditService
}

type SellerInput struct {
	Code         string `json:"code" binding:"required"`
	Name         string `json:"name" binding:"required"`
	ContactEmail string `json:"contact_email"`
	ContactPhone string `json:"contact_phone"`
	Status       string `json:"status"`
	Note         string `json:"note"`
}

type StoreInput struct {
	SellerID    uint   `json:"seller_id" binding:"required"`
	Name        string `json:"name" binding:"required"`
	Platform    string `json:"platform"`
	ExternalRef string `json:"external_ref"`
}

func (s *SellerService) Create(actor Actor, in SellerInput) (*models.Seller, error) {
	in.Code = strings.ToUpper(strings.TrimSpace(in.Code))
	if _, err := s.repo.Seller.FindByCode(in.Code); err == nil {
		return nil, apperr.Conflict("A seller with this code already exists")
	}
	status := in.Status
	if status == "" {
		status = "active"
	}
	seller := &models.Seller{
		Code: in.Code, Name: in.Name, ContactEmail: in.ContactEmail,
		ContactPhone: in.ContactPhone, Status: status, Note: in.Note,
	}
	if err := s.repo.Seller.Create(seller); err != nil {
		return nil, apperr.Internal("could not create seller").Wrap(err)
	}
	s.audit.Log(actor, "SELLER_CREATE", "seller", &seller.ID, "Created seller "+seller.Code, nil)
	return seller, nil
}

func (s *SellerService) Get(id uint) (*models.Seller, error) {
	seller, err := s.repo.Seller.FindByID(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperr.NotFound("Seller not found")
		}
		return nil, apperr.Internal("lookup failed").Wrap(err)
	}
	return seller, nil
}

func (s *SellerService) List(page repositories.Page) ([]models.Seller, int64, error) {
	return s.repo.Seller.List(page.Normalize())
}

func (s *SellerService) Update(actor Actor, id uint, in SellerInput) (*models.Seller, error) {
	seller, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if in.Name != "" {
		seller.Name = in.Name
	}
	seller.ContactEmail = in.ContactEmail
	seller.ContactPhone = in.ContactPhone
	if in.Status != "" {
		seller.Status = in.Status
	}
	seller.Note = in.Note
	if err := s.repo.Seller.Update(seller); err != nil {
		return nil, apperr.Internal("could not update seller").Wrap(err)
	}
	s.audit.Log(actor, "SELLER_UPDATE", "seller", &seller.ID, "Updated seller "+seller.Code, nil)
	return seller, nil
}

func (s *SellerService) Delete(actor Actor, id uint) error {
	if _, err := s.Get(id); err != nil {
		return err
	}
	if err := s.repo.Seller.Delete(id); err != nil {
		return apperr.Internal("could not delete seller").Wrap(err)
	}
	s.audit.Log(actor, "SELLER_DELETE", "seller", &id, "Deleted seller", nil)
	return nil
}

// ---------- Stores ----------

func (s *SellerService) CreateStore(actor Actor, in StoreInput) (*models.Store, error) {
	if _, err := s.repo.Seller.FindByID(in.SellerID); err != nil {
		return nil, apperr.BadRequest("seller_id does not reference an existing seller")
	}
	store := &models.Store{
		SellerID: in.SellerID, Name: in.Name, Platform: in.Platform, ExternalRef: in.ExternalRef,
	}
	if err := s.repo.Store.Create(store); err != nil {
		return nil, apperr.Internal("could not create store").Wrap(err)
	}
	s.audit.Log(actor, "STORE_CREATE", "store", &store.ID, "Created store "+store.Name, nil)
	return store, nil
}

func (s *SellerService) GetStore(id uint) (*models.Store, error) {
	store, err := s.repo.Store.FindByID(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperr.NotFound("Store not found")
		}
		return nil, apperr.Internal("lookup failed").Wrap(err)
	}
	return store, nil
}

func (s *SellerService) ListStores(page repositories.Page, sellerID *uint) ([]models.Store, int64, error) {
	return s.repo.Store.List(page.Normalize(), sellerID)
}

func (s *SellerService) UpdateStore(actor Actor, id uint, in StoreInput) (*models.Store, error) {
	store, err := s.GetStore(id)
	if err != nil {
		return nil, err
	}
	if in.Name != "" {
		store.Name = in.Name
	}
	store.Platform = in.Platform
	store.ExternalRef = in.ExternalRef
	if err := s.repo.Store.Update(store); err != nil {
		return nil, apperr.Internal("could not update store").Wrap(err)
	}
	s.audit.Log(actor, "STORE_UPDATE", "store", &store.ID, "Updated store "+store.Name, nil)
	return store, nil
}

func (s *SellerService) DeleteStore(actor Actor, id uint) error {
	if _, err := s.GetStore(id); err != nil {
		return err
	}
	if err := s.repo.Store.Delete(id); err != nil {
		return apperr.Internal("could not delete store").Wrap(err)
	}
	s.audit.Log(actor, "STORE_DELETE", "store", &id, "Deleted store", nil)
	return nil
}
