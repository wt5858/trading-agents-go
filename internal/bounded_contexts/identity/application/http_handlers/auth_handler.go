// Package http_handlers exposes the identity context over HTTP.
//
// Handlers only bind and shape-check the request, delegate to a domain_service, and
// render the envelope. No business rule lives here.
package http_handlers

import (
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/identity/domain_services"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/identity/entities"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
	"github.com/wt5858/trading-agents-go/internal/helpers/response"
)

// ContextKeyClaims is where the auth middleware stores the caller's identity.
const ContextKeyClaims = "identity.claims"

type AuthHandler struct {
	authService *domain_services.AuthService
}

func NewAuthHandler(authService *domain_services.AuthService) *AuthHandler {
	return &AuthHandler{authService: authService}
}

func (h *AuthHandler) Register(rg *gin.RouterGroup, authRequired gin.HandlerFunc) {
	rg.POST("/auth/login", h.Login)
	rg.POST("/auth/refresh", h.Refresh)
	rg.POST("/auth/logout", authRequired, h.Logout)
	rg.GET("/auth/me", authRequired, h.Me)
}

// loginRequest 用户名口令登录。
type loginRequest struct {
	Username string `json:"username" binding:"required" example:"admin"`
	Password string `json:"password" binding:"required" example:"secret"`
} // @name identity.LoginRequest

// tokenResponse 一次成功登录的全部产物。
type tokenResponse struct {
	AccessToken  string    `json:"accessToken"`
	RefreshToken string    `json:"refreshToken"`
	TokenType    string    `json:"tokenType" example:"Bearer"`
	ExpiresIn    int64     `json:"expiresIn" example:"3600"`
	User         *userView `json:"user"`
} // @name identity.TokenResponse

// Login 登录并换取访问令牌。
//
// @Summary  登录
// @Tags     认证
// @Accept   json
// @Produce  json
// @Param    body body     loginRequest true "用户名与口令"
// @Success  200  {object} response.Envelope{data=tokenResponse}
// @Failure  400  {object} response.Envelope "请求参数不合法"
// @Failure  401  {object} response.Envelope "用户名或口令错误"
// @Router   /auth/login [post]
func (h *AuthHandler) Login(c *gin.Context) {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, custom_errors.Invalid("请求参数不合法: %v", err))
		return
	}

	result, err := h.authService.Login(c.Request.Context(), req.Username, req.Password)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, toTokenResponse(result))
}

// refreshRequest 用刷新令牌换一对新令牌。
type refreshRequest struct {
	RefreshToken string `json:"refreshToken" binding:"required"`
} // @name identity.RefreshRequest

// Refresh 刷新访问令牌。
//
// @Summary  刷新令牌
// @Tags     认证
// @Accept   json
// @Produce  json
// @Param    body body     refreshRequest true "刷新令牌"
// @Success  200  {object} response.Envelope{data=tokenResponse}
// @Failure  400  {object} response.Envelope "请求参数不合法"
// @Failure  401  {object} response.Envelope "刷新令牌无效或已过期"
// @Router   /auth/refresh [post]
func (h *AuthHandler) Refresh(c *gin.Context) {
	var req refreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, custom_errors.Invalid("请求参数不合法: %v", err))
		return
	}
	result, err := h.authService.Refresh(c.Request.Context(), req.RefreshToken)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, toTokenResponse(result))
}

// Logout 吊销当前会话。
//
// @Summary  登出
// @Tags     认证
// @Produce  json
// @Security BearerAuth
// @Success  200 {object} response.Envelope{data=logoutView}
// @Failure  401 {object} response.Envelope "未登录"
// @Router   /auth/logout [post]
func (h *AuthHandler) Logout(c *gin.Context) {
	claims := ClaimsOf(c)
	if claims == nil {
		response.Fail(c, custom_errors.Unauthorized("未登录"))
		return
	}
	if err := h.authService.Logout(c.Request.Context(), claims.SessionID); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, logoutView{LoggedOut: true})
}

// Me 返回当前令牌所代表的身份。
//
// @Summary  当前登录身份
// @Tags     认证
// @Produce  json
// @Security BearerAuth
// @Success  200 {object} response.Envelope{data=meView}
// @Failure  401 {object} response.Envelope "未登录"
// @Router   /auth/me [get]
func (h *AuthHandler) Me(c *gin.Context) {
	claims := ClaimsOf(c)
	if claims == nil {
		response.Fail(c, custom_errors.Unauthorized("未登录"))
		return
	}
	response.OK(c, meView{
		UserID:   claims.UserID,
		Username: claims.Username,
		Role:     claims.Role.String(),
	})
}

// ClaimsOf reads the authenticated caller out of the gin context.
func ClaimsOf(c *gin.Context) *domain_services.Claims {
	v, ok := c.Get(ContextKeyClaims)
	if !ok {
		return nil
	}
	claims, _ := v.(*domain_services.Claims)
	return claims
}

// BearerToken extracts the token from the Authorization header.
func BearerToken(c *gin.Context) string {
	h := c.GetHeader("Authorization")
	if h == "" {
		return ""
	}
	const prefix = "Bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return h[len(prefix):]
	}
	return h
}

// logoutView 登出结果。
//
// 一个布尔值本可以用 gin.H 写完，但那样它就不在 OpenAPI 里——
// 响应体的形状只要没有类型，文档就只能靠人手写，而手写的那份迟早和代码分家。
type logoutView struct {
	LoggedOut bool `json:"loggedOut" example:"true"`
} // @name identity.LogoutView

// meView 当前登录身份。
type meView struct {
	UserID   uint64 `json:"userId"`
	Username string `json:"username"`
	Role     string `json:"role" example:"admin"`
} // @name identity.MeView

// userView 用户视图。
type userView struct {
	ID              uint64 `json:"id"`
	Username        string `json:"username"`
	Email           string `json:"email"`
	Role            string `json:"role"`
	Active          bool   `json:"active"`
	ConcurrentLimit int    `json:"concurrentLimit"`
	Preferences     any    `json:"preferences"`
} // @name identity.UserView

func toUserView(u *entities.User) *userView {
	if u == nil {
		return nil
	}
	return &userView{
		ID:              u.ID,
		Username:        u.Username.String(),
		Email:           u.Email,
		Role:            u.Role.String(),
		Active:          u.Active,
		ConcurrentLimit: u.ConcurrentLimit,
		Preferences:     u.Preferences,
	}
}

func toTokenResponse(r *domain_services.LoginResult) tokenResponse {
	return tokenResponse{
		AccessToken:  r.Tokens.AccessToken,
		RefreshToken: r.Tokens.RefreshToken,
		TokenType:    r.Tokens.TokenType,
		ExpiresIn:    r.Tokens.ExpiresIn,
		User:         toUserView(r.User),
	}
}
