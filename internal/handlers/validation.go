package handlers

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/gin-gonic/gin/binding"
	"github.com/go-playground/validator/v10"
)

// init teaches gin's validator to report the JSON field name (e.g. "code")
// instead of the Go struct field name (e.g. "Code"), so error messages line up
// with what the frontend actually sends.
func init() {
	if v, ok := binding.Validator.Engine().(*validator.Validate); ok {
		v.RegisterTagNameFunc(func(fld reflect.StructField) string {
			name := strings.SplitN(fld.Tag.Get("json"), ",", 2)[0]
			if name == "-" || name == "" {
				return fld.Name
			}
			return name
		})
	}
}

// fieldLabels maps JSON field names to human-friendly Vietnamese labels shown
// to the user. Fields not listed fall back to their JSON name.
var fieldLabels = map[string]string{
	"code":              "Mã",
	"name":              "Tên",
	"product_name":      "Tên sản phẩm",
	"description":       "Mô tả",
	"materials":         "Danh sách nguyên vật liệu",
	"material_id":       "Nguyên vật liệu",
	"quantity_per_unit": "Số lượng mỗi đơn vị",
	"note":              "Ghi chú",
	"email":             "Email",
	"password":          "Mật khẩu",
	"is_active":         "Trạng thái kích hoạt",
}

func fieldLabel(jsonName string) string {
	// Strip any array index the validator adds for -dive- errors, e.g.
	// "materials[0].material_id" -> "material_id".
	if i := strings.LastIndex(jsonName, "."); i >= 0 {
		jsonName = jsonName[i+1:]
	}
	if label, ok := fieldLabels[jsonName]; ok {
		return label
	}
	return jsonName
}

// ruleMessage turns a single validator FieldError into a Vietnamese reason.
func ruleMessage(fe validator.FieldError) string {
	switch fe.Tag() {
	case "required":
		return "bắt buộc nhập"
	case "email":
		return "email không hợp lệ"
	case "min":
		if fe.Kind() == reflect.String {
			return fmt.Sprintf("phải có ít nhất %s ký tự", fe.Param())
		}
		return fmt.Sprintf("phải lớn hơn hoặc bằng %s", fe.Param())
	case "max":
		if fe.Kind() == reflect.String {
			return fmt.Sprintf("tối đa %s ký tự", fe.Param())
		}
		return fmt.Sprintf("phải nhỏ hơn hoặc bằng %s", fe.Param())
	case "oneof":
		return fmt.Sprintf("phải là một trong các giá trị: %s", fe.Param())
	case "len":
		return fmt.Sprintf("phải có độ dài %s", fe.Param())
	case "gte":
		return fmt.Sprintf("phải lớn hơn hoặc bằng %s", fe.Param())
	case "lte":
		return fmt.Sprintf("phải nhỏ hơn hoặc bằng %s", fe.Param())
	default:
		return "không hợp lệ"
	}
}

// bindFieldError is one field-level entry returned in the error `details`.
type bindFieldError struct {
	Field   string `json:"field"`
	Label   string `json:"label"`
	Message string `json:"message"`
}

// humanizeBindErr converts a gin binding error into a user-readable message plus
// structured per-field details. Falls back gracefully for non-validation errors
// (e.g. malformed JSON) so the caller always has something meaningful to show.
func humanizeBindErr(err error) (message string, details interface{}) {
	var verrs validator.ValidationErrors
	if !errors.As(err, &verrs) || len(verrs) == 0 {
		// Not a field-validation error (malformed JSON, wrong type, …).
		return "Dữ liệu gửi lên không đúng định dạng.", err.Error()
	}

	fields := make([]bindFieldError, 0, len(verrs))
	parts := make([]string, 0, len(verrs))
	for _, fe := range verrs {
		label := fieldLabel(fe.Field())
		reason := ruleMessage(fe)
		fields = append(fields, bindFieldError{Field: fe.Field(), Label: label, Message: reason})
		parts = append(parts, fmt.Sprintf("%s (%s)", label, reason))
	}
	return "Dữ liệu chưa hợp lệ: " + strings.Join(parts, ", "), fields
}
