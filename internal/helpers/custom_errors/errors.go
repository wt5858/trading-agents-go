// Package apperr 定义类型化领域错误。
//
// 分层约定：错误在规则所在处产生——也就是 entities/ 与 value_objects/。
// application/ 不构造业务校验错误，只做透传与权限判定；
// repositories/ 只负责把 gorm/mongo 的技术错误翻译成这里的类型。
package custom_errors

import (
	"errors"
	"fmt"
)

// Code 是稳定的领域错误码，接口层据此映射 HTTP 状态码与业务码。
type Code string

const (
	CodeInvalidArgument Code = "INVALID_ARGUMENT"
	CodeNotFound        Code = "NOT_FOUND"
	CodeAlreadyExists   Code = "ALREADY_EXISTS"
	CodeUnauthorized    Code = "UNAUTHORIZED"
	CodeForbidden       Code = "FORBIDDEN"
	CodeConflict        Code = "CONFLICT"
	CodeQuotaExceeded   Code = "QUOTA_EXCEEDED"
	CodeUnavailable     Code = "UNAVAILABLE"
	CodeInternal        Code = "INTERNAL"
)

// Error 是领域错误。领域层只抛这一种错误类型。
type Error struct {
	Code    Code
	Message string
	cause   error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.cause }

// Wrap 在保留错误码的前提下附加底层原因。
func (e *Error) Wrap(cause error) *Error {
	return &Error{Code: e.Code, Message: e.Message, cause: cause}
}

func newError(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func Invalid(format string, args ...any) *Error {
	return newError(CodeInvalidArgument, format, args...)
}
func NotFound(format string, args ...any) *Error { return newError(CodeNotFound, format, args...) }
func AlreadyExists(format string, args ...any) *Error {
	return newError(CodeAlreadyExists, format, args...)
}
func Unauthorized(format string, args ...any) *Error {
	return newError(CodeUnauthorized, format, args...)
}
func Forbidden(format string, args ...any) *Error { return newError(CodeForbidden, format, args...) }
func Conflict(format string, args ...any) *Error  { return newError(CodeConflict, format, args...) }
func QuotaExceeded(format string, args ...any) *Error {
	return newError(CodeQuotaExceeded, format, args...)
}
func Unavailable(format string, args ...any) *Error {
	return newError(CodeUnavailable, format, args...)
}
func Internal(format string, args ...any) *Error { return newError(CodeInternal, format, args...) }

// CodeOf 提取错误码，非领域错误一律视为内部错误。
func CodeOf(err error) Code {
	var de *Error
	if errors.As(err, &de) {
		return de.Code
	}
	return CodeInternal
}

// MessageOf 提取可安全展示给调用方的消息。
// 非领域错误不暴露内部细节，避免把 SQL 语句之类的内容泄露到响应里。
func MessageOf(err error) string {
	var de *Error
	if errors.As(err, &de) {
		return de.Message
	}
	return "服务内部错误"
}
