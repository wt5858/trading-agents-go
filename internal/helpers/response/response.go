// Package response implements the mandatory API envelope: every response, success or
// failure, is {"code": int, "message": string, "data": any}.
package response

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// Envelope is the only shape this service ever writes to the wire.
type Envelope struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

// CodeOK is the success code. Everything non-zero is a business failure.
const CodeOK = 0

// Business codes are HTTP-status-prefixed (404xx, 403xx …) so an operator reading a log
// line can tell the class of failure without a lookup table, while the numeric space
// stays open for finer-grained codes per domain error.
const (
	CodeInvalidArgument = 40000
	CodeUnauthorized    = 40100
	CodeForbidden       = 40300
	CodeNotFound        = 40400
	CodeConflict        = 40900
	CodeAlreadyExists   = 40901
	CodeQuotaExceeded   = 42900
	CodeInternal        = 50000
	CodeUnavailable     = 50300
)

// PageData is the standard envelope payload for paginated lists.
type PageData struct {
	Items    any   `json:"items"`
	Total    int64 `json:"total"`
	Page     int   `json:"page"`
	PageSize int   `json:"pageSize"`
}

// OK writes a success envelope.
func OK(c *gin.Context, data any) {
	c.JSON(http.StatusOK, Envelope{Code: CodeOK, Message: "ok", Data: data})
}

// OKPage writes a success envelope wrapping a paginated list.
func OKPage(c *gin.Context, items any, total int64, page, pageSize int) {
	OK(c, PageData{Items: items, Total: total, Page: page, PageSize: pageSize})
}

// Fail maps a domain error onto the envelope and the matching HTTP status.
//
// The HTTP status is set as well as the business code: load balancers, dashboards and
// client retry logic all key off status, and collapsing everything to 200 makes real
// outages invisible in monitoring.
func Fail(c *gin.Context, err error) {
	bizCode, status := classify(custom_errors.CodeOf(err))
	c.AbortWithStatusJSON(status, Envelope{
		Code:    bizCode,
		Message: custom_errors.MessageOf(err),
		Data:    nil,
	})
}

// FailBind turns a ShouldBind* error into a client-safe InvalidArgument error.
//
// # Why this exists instead of `Invalid("请求参数不合法: %v", err)`
//
// go-playground/validator's Error() reads
//
//	Key: 'loginRequest.Password' Error:Field validation for 'Password' failed on the 'required' tag
//
// and MessageOf hands a domain error's Message to the wire verbatim. So that one `%v`
// published our internal struct names — the request type, its field layout, the
// validation tags in use — on every malformed request. It is the one place the
// otherwise careful redaction in MessageOf is bypassed by construction, and it was
// copy-pasted across twenty-odd handlers.
//
// What goes out now is the offending field names and nothing else: enough for a client
// to fix its request, nothing about how the server is put together. A non-validator
// bind failure (malformed JSON, wrong type) carries no field list at all, because its
// message is the decoder's and we do not control what it contains.
//
// The full error stays available to the caller if it wants to log it; it just never
// reaches the response body.
func FailBind(c *gin.Context, err error) {
	Fail(c, bindError(err))
}

func bindError(err error) error {
	var ve validator.ValidationErrors
	if errors.As(err, &ve) && len(ve) > 0 {
		fields := make([]string, 0, len(ve))
		for _, fe := range ve {
			fields = append(fields, fe.Field())
		}
		return custom_errors.Invalid("请求参数不合法：%s", strings.Join(fields, "、")).Wrap(err)
	}
	return custom_errors.Invalid("请求参数不合法").Wrap(err)
}

// FailWith writes an explicit code and message, for cases with no underlying error.
func FailWith(c *gin.Context, bizCode int, status int, message string) {
	c.AbortWithStatusJSON(status, Envelope{Code: bizCode, Message: message, Data: nil})
}

func classify(code custom_errors.Code) (bizCode int, httpStatus int) {
	switch code {
	case custom_errors.CodeInvalidArgument:
		return CodeInvalidArgument, http.StatusBadRequest
	case custom_errors.CodeUnauthorized:
		return CodeUnauthorized, http.StatusUnauthorized
	case custom_errors.CodeForbidden:
		return CodeForbidden, http.StatusForbidden
	case custom_errors.CodeNotFound:
		return CodeNotFound, http.StatusNotFound
	case custom_errors.CodeAlreadyExists:
		return CodeAlreadyExists, http.StatusConflict
	case custom_errors.CodeConflict:
		return CodeConflict, http.StatusConflict
	case custom_errors.CodeQuotaExceeded:
		return CodeQuotaExceeded, http.StatusTooManyRequests
	case custom_errors.CodeUnavailable:
		return CodeUnavailable, http.StatusServiceUnavailable
	default:
		return CodeInternal, http.StatusInternalServerError
	}
}
