package handlers

import (
	"mime/multipart"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/response"
)

// isMultipart reports whether an import request carries a file rather than
// JSON rows.
func isMultipart(c *gin.Context) bool {
	return strings.HasPrefix(c.ContentType(), "multipart/form-data")
}

// spreadsheetUpload opens the multipart "file" field of an import request and
// names its format for the parsers: "XLSX" (.xlsx/.xlsm) or "CSV" (anything
// else). Old binary .xls is refused with a hint. ok=false means the response
// has already been written; on ok the caller owns f and must Close it.
func spreadsheetUpload(c *gin.Context) (source, filename string, f multipart.File, ok bool) {
	fileHeader, err := c.FormFile("file")
	if err != nil {
		response.Fail(c, apperr.BadRequest(`Thiếu file upload (field "file")`))
		return "", "", nil, false
	}
	f, err = fileHeader.Open()
	if err != nil {
		response.Fail(c, apperr.BadRequest("Không mở được file upload"))
		return "", "", nil, false
	}
	filename = fileHeader.Filename
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".xlsx", ".xlsm":
		source = "XLSX"
	case ".xls":
		f.Close()
		response.Fail(c, apperr.BadRequest("Định dạng .xls (Excel cũ) chưa hỗ trợ — lưu lại dạng .xlsx hoặc CSV"))
		return "", "", nil, false
	default:
		source = "CSV"
	}
	return source, filename, f, true
}
