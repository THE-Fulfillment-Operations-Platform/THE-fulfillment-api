package services

import (
	"fmt"
	"sort"
	"strconv"
	"time"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// maxImportTemplateExportItems caps one "export as import template" download: the
// workbook is built in memory, and a filter left wide open over the whole history
// should be narrowed rather than produce a file nobody can open.
const maxImportTemplateExportItems = 10000

// ExportItemsAsImportXLSX writes the items matching f in the ORDER IMPORT TEMPLATE
// layout — the very columns of OrderImportTemplateXLSX — so the file can be edited
// and imported straight back (e.g. to move test orders to another seller, or to
// recreate an order). One row per item, rows of one order kept together in line
// order, the order's recipient block repeated on every row as the import expects.
// Technician-only: it hands out recipients' addresses in bulk.
func (s *OrderService) ExportItemsAsImportXLSX(actor Actor, f repositories.ItemFilter) ([]byte, string, error) {
	if actor.Role != models.RoleTechnician {
		return nil, "", apperr.Forbidden("Chỉ kỹ thuật viên được xuất đơn theo mẫu import")
	}
	rows, err := s.repo.OrderItem.ListAll(f)
	if err != nil {
		return nil, "", apperr.Internal("could not load items").Wrap(err)
	}
	if len(rows) > maxImportTemplateExportItems {
		return nil, "", apperr.BadRequest(fmt.Sprintf(
			"Bộ lọc khớp %d sản phẩm — tối đa %d mỗi lần xuất, hãy lọc hẹp lại (seller, ngày…)",
			len(rows), maxImportTemplateExportItems))
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].OrderID != rows[j].OrderID {
			return rows[i].OrderID < rows[j].OrderID
		}
		return rows[i].LineNo < rows[j].LineNo
	})
	grid := make([][]string, 0, len(rows)+1)
	grid = append(grid, orderImportTemplateHeaders)
	for _, it := range rows {
		grid = append(grid, importTemplateRow(&it))
	}
	data, err := buildTemplateXLSX("Đơn hàng", grid, orderImportTemplateWidths)
	if err != nil {
		return nil, "", err
	}
	return data, "orders-import-" + AppDateString(time.Now()) + ".xlsx", nil
}

// importTemplateRow is one item in orderImportTemplateHeaders order.
func importTemplateRow(it *models.OrderItem) []string {
	o := it.Order
	if o == nil {
		o = &models.Order{}
	}
	qty := it.Quantity
	if qty < 1 {
		qty = 1
	}
	return []string{
		o.Seller.Code, o.Account, o.StoreName, o.OrderDate, o.ShippingName,
		o.ShippingAddress1, o.ShippingAddress2, o.ShippingCity, o.ShippingProvince, o.ShippingZip, o.ShippingCountry,
		o.StoreOrderID, it.SKUCode, it.ImageCode, strconv.Itoa(qty),
		it.DesignURL, it.BackDesignURL, it.MockupURL, it.EngraveText,
		o.ShippingPhone, o.Note,
	}
}
