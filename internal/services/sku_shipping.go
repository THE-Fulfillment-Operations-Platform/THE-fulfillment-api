package services

import (
	"fmt"
	"math"
	"strings"
	"unicode"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
)

// Shipping declaration of a SKU (see models.SKU.ShipWeightG…): the numbers the
// carrier needs to create a shipment for ONE unit. Every field is optional on
// its own — a child SKU leaves blank what it inherits from its parent.

// Bounds are sanity caps, not carrier rules: they catch a weight typed in kg
// where grams were meant, or a size typed in mm where cm were meant, before the
// number reaches a paid API.
const (
	maxShipWeightG     = 30000 // 30 kg for one unit of a decor product
	maxShipDimCM       = 200
	maxDeclaredValue   = 5000 // USD
	minHSCodeDigits    = 6
	maxHSCodeDigits    = 13
	shipValueRounding  = 100 // value kept to the cent
	shipNumberRounding = 10  // weight/size kept to 0.1
)

// SKUShippingInput is the shipping part of a SKU create/update payload.
// Pointer semantics like the D×R pair: omitted = leave as is, ≤0 = clear (the
// SKU then inherits that field from its parent). HSCode: omitted = leave,
// "" = clear.
type SKUShippingInput struct {
	ShipWeightG   *float64 `json:"ship_weight_g"`
	ShipLengthCM  *float64 `json:"ship_length_cm"`
	ShipWidthCM   *float64 `json:"ship_width_cm"`
	ShipHeightCM  *float64 `json:"ship_height_cm"`
	DeclaredValue *float64 `json:"declared_value"`
	HSCode        *string  `json:"hs_code"`
}

// applySKUShipping validates in and writes the fields it carries onto sku.
func applySKUShipping(sku *models.SKU, in SKUShippingInput) error {
	var err error
	if in.ShipWeightG != nil {
		if sku.ShipWeightG, err = shipNumber(*in.ShipWeightG, maxShipWeightG, shipNumberRounding,
			fmt.Sprintf("Cân nặng phải nhỏ hơn %d g (nhập theo gram)", maxShipWeightG)); err != nil {
			return err
		}
	}
	sizeMsg := fmt.Sprintf("Kích thước hộp phải nhỏ hơn %d cm (nhập theo cm)", maxShipDimCM)
	if in.ShipLengthCM != nil {
		if sku.ShipLengthCM, err = shipNumber(*in.ShipLengthCM, maxShipDimCM, shipNumberRounding, sizeMsg); err != nil {
			return err
		}
	}
	if in.ShipWidthCM != nil {
		if sku.ShipWidthCM, err = shipNumber(*in.ShipWidthCM, maxShipDimCM, shipNumberRounding, sizeMsg); err != nil {
			return err
		}
	}
	if in.ShipHeightCM != nil {
		if sku.ShipHeightCM, err = shipNumber(*in.ShipHeightCM, maxShipDimCM, shipNumberRounding, sizeMsg); err != nil {
			return err
		}
	}
	if in.DeclaredValue != nil {
		if sku.DeclaredValue, err = shipNumber(*in.DeclaredValue, maxDeclaredValue, shipValueRounding,
			fmt.Sprintf("Giá trị khai báo phải nhỏ hơn %d USD", maxDeclaredValue)); err != nil {
			return err
		}
	}
	if in.HSCode != nil {
		code, err := normalizeHSCode(*in.HSCode)
		if err != nil {
			return err
		}
		sku.HSCode = code
	}
	return nil
}

// shipNumber: ≤0 clears (nil), otherwise the value must be under max and is
// rounded to 1/rounding.
func shipNumber(v float64, max float64, rounding float64, tooBig string) (*float64, error) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil, apperr.BadRequest("Số không hợp lệ")
	}
	if v <= 0 {
		return nil, nil
	}
	if v > max {
		return nil, apperr.BadRequest(tooBig)
	}
	r := math.Round(v*rounding) / rounding
	return &r, nil
}

// normalizeHSCode keeps the digits of an HS code ("3926.40.00" → "39264000")
// and checks the length a customs code can have.
func normalizeHSCode(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	var b strings.Builder
	for _, r := range raw {
		switch {
		case unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '.' || r == ' ' || r == '-':
			// separators people type: dropped
		default:
			return "", apperr.BadRequest(fmt.Sprintf("Mã HS %q chỉ được có chữ số", raw))
		}
	}
	code := b.String()
	if len(code) < minHSCodeDigits || len(code) > maxHSCodeDigits {
		return "", apperr.BadRequest(fmt.Sprintf("Mã HS %q phải có %d–%d chữ số", raw, minHSCodeDigits, maxHSCodeDigits))
	}
	return code, nil
}
