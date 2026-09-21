package http_handlers

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/identity/domain_services"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/identity/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
	"github.com/wt5858/trading-agents-go/internal/helpers/response"
)

type UserHandler struct {
	userService *domain_services.UserService
}

func NewUserHandler(userService *domain_services.UserService) *UserHandler {
	return &UserHandler{userService: userService}
}

func (h *UserHandler) Register(rg *gin.RouterGroup, authRequired gin.HandlerFunc) {
	users := rg.Group("/users", authRequired)
	users.GET("", h.List)
	users.POST("", h.Create)
	users.GET("/:id", h.Get)
	users.PUT("/profile", h.UpdateProfile)
	users.POST("/password", h.ChangePassword)
	users.POST("/:id/password/reset", h.ResetPassword)
	users.POST("/:id/deactivate", h.Deactivate)
}

// List 分页查询用户。
//
// @Summary  用户列表
// @Tags     用户
// @Produce  json
// @Security BearerAuth
// @Param    keyword  query    string false "用户名关键字，模糊匹配"
// @Param    page     query    int    false "页码，缺省 1"
// @Param    pageSize query    int    false "每页条数，缺省 20"
// @Success  200      {object} response.Envelope{data=response.PageData{items=[]userView}}
// @Failure  401      {object} response.Envelope "未登录或令牌无效"
// @Failure  403      {object} response.Envelope "需要管理员权限"
// @Router   /users [get]
func (h *UserHandler) List(c *gin.Context) {
	page := parsePage(c)
	users, total, err := h.userService.List(c.Request.Context(), ClaimsOf(c), c.Query("keyword"), page)
	if err != nil {
		response.Fail(c, err)
		return
	}
	views := make([]*userView, 0, len(users))
	for _, u := range users {
		views = append(views, toUserView(u))
	}
	response.OKPage(c, views, total, page.Number, page.Size)
}

// createUserRequest 管理员建号。Role 留空时由领域层落到 user。
type createUserRequest struct {
	Username string `json:"username" binding:"required" example:"trader01"`
	Email    string `json:"email" example:"trader01@example.com"`
	Password string `json:"password" binding:"required"`
	Role     string `json:"role" example:"user" enums:"user,admin"`
} // @name identity.CreateUserRequest

// Create 新建用户。
//
// @Summary  新建用户
// @Tags     用户
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    body body     createUserRequest true "用户名、口令与角色"
// @Success  200  {object} response.Envelope{data=userView}
// @Failure  400  {object} response.Envelope "请求参数不合法"
// @Failure  401  {object} response.Envelope "未登录或令牌无效"
// @Failure  403  {object} response.Envelope "需要管理员权限"
// @Failure  409  {object} response.Envelope "用户名已被占用"
// @Router   /users [post]
func (h *UserHandler) Create(c *gin.Context) {
	var req createUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, custom_errors.Invalid("请求参数不合法: %v", err))
		return
	}
	user, err := h.userService.CreateUser(c.Request.Context(), ClaimsOf(c), domain_services.CreateUserCommand{
		Username: req.Username,
		Email:    req.Email,
		Password: req.Password,
		Role:     req.Role,
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, toUserView(user))
}

// Get 取单个用户。
//
// @Summary  查询用户
// @Tags     用户
// @Produce  json
// @Security BearerAuth
// @Param    id  path     int true "用户 ID"
// @Success  200 {object} response.Envelope{data=userView}
// @Failure  400 {object} response.Envelope "用户 ID 不是正整数"
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Failure  403 {object} response.Envelope "只能查看自己，除非是管理员"
// @Failure  404 {object} response.Envelope "用户不存在"
// @Router   /users/{id} [get]
func (h *UserHandler) Get(c *gin.Context) {
	id, err := parseUintParam(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	claims := ClaimsOf(c)
	// A caller may always read themselves; reading anyone else needs admin rights.
	if claims == nil || (claims.UserID != id && !claims.Role.IsAdmin()) {
		response.Fail(c, custom_errors.Forbidden("无权查看该用户"))
		return
	}
	user, err := h.userService.GetByID(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, toUserView(user))
}

// updateProfileRequest 部分更新：留空的字段不会被改写。
type updateProfileRequest struct {
	Email       string                     `json:"email" example:"trader01@example.com"`
	Preferences *value_objects.Preferences `json:"preferences"`
} // @name identity.UpdateProfileRequest

// UpdateProfile 改当前登录者的资料。
//
// @Summary  更新个人资料
// @Tags     用户
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    body body     updateProfileRequest true "待更新的邮箱与偏好"
// @Success  200  {object} response.Envelope{data=userView}
// @Failure  400  {object} response.Envelope "请求参数不合法"
// @Failure  401  {object} response.Envelope "未登录或令牌无效"
// @Failure  404  {object} response.Envelope "用户不存在"
// @Router   /users/profile [put]
func (h *UserHandler) UpdateProfile(c *gin.Context) {
	claims := ClaimsOf(c)
	if claims == nil {
		response.Fail(c, custom_errors.Unauthorized("未登录"))
		return
	}
	var req updateProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, custom_errors.Invalid("请求参数不合法: %v", err))
		return
	}
	user, err := h.userService.UpdateProfile(c.Request.Context(), claims.UserID, domain_services.UpdateProfileCommand{
		Email:       req.Email,
		Preferences: req.Preferences,
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, toUserView(user))
}

// changePasswordRequest 自助改密，需要原口令。
type changePasswordRequest struct {
	OldPassword string `json:"oldPassword" binding:"required"`
	NewPassword string `json:"newPassword" binding:"required"`
} // @name identity.ChangePasswordRequest

// ChangePassword 用原口令换新口令，成功后旧令牌立即失效。
//
// @Summary  修改密码
// @Tags     用户
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    body body     changePasswordRequest true "原口令与新口令"
// @Success  200  {object} response.Envelope{data=changePasswordView}
// @Failure  400  {object} response.Envelope "请求参数不合法或新口令强度不足"
// @Failure  401  {object} response.Envelope "未登录、令牌无效或原密码不正确"
// @Failure  404  {object} response.Envelope "用户不存在"
// @Router   /users/password [post]
func (h *UserHandler) ChangePassword(c *gin.Context) {
	claims := ClaimsOf(c)
	if claims == nil {
		response.Fail(c, custom_errors.Unauthorized("未登录"))
		return
	}
	var req changePasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, custom_errors.Invalid("请求参数不合法: %v", err))
		return
	}
	if err := h.userService.ChangePassword(c.Request.Context(), claims.UserID, req.OldPassword, req.NewPassword); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, changePasswordView{Changed: true})
}

// resetPasswordRequest 管理员重置他人口令，不需要原口令。
type resetPasswordRequest struct {
	NewPassword string `json:"newPassword" binding:"required"`
} // @name identity.ResetPasswordRequest

// ResetPassword 由管理员重置指定用户的口令。
//
// @Summary  重置密码
// @Tags     用户
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path     int                  true "用户 ID"
// @Param    body body     resetPasswordRequest true "新口令"
// @Success  200  {object} response.Envelope{data=resetPasswordView}
// @Failure  400  {object} response.Envelope "用户 ID 不是正整数，或新口令强度不足"
// @Failure  401  {object} response.Envelope "未登录或令牌无效"
// @Failure  403  {object} response.Envelope "需要管理员权限"
// @Failure  404  {object} response.Envelope "用户不存在"
// @Router   /users/{id}/password/reset [post]
func (h *UserHandler) ResetPassword(c *gin.Context) {
	id, err := parseUintParam(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	var req resetPasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, custom_errors.Invalid("请求参数不合法: %v", err))
		return
	}
	if err := h.userService.ResetPassword(c.Request.Context(), ClaimsOf(c), id, req.NewPassword); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, resetPasswordView{Reset: true})
}

// Deactivate 停用账号，并吊销该用户的全部会话。
//
// @Summary  停用用户
// @Tags     用户
// @Produce  json
// @Security BearerAuth
// @Param    id  path     int true "用户 ID"
// @Success  200 {object} response.Envelope{data=deactivateView}
// @Failure  400 {object} response.Envelope "用户 ID 不是正整数"
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Failure  403 {object} response.Envelope "需要管理员权限，且不能停用自己"
// @Failure  404 {object} response.Envelope "用户不存在"
// @Failure  409 {object} response.Envelope "账号已停用，或系统必须保留至少一个启用的管理员"
// @Router   /users/{id}/deactivate [post]
func (h *UserHandler) Deactivate(c *gin.Context) {
	id, err := parseUintParam(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	if err := h.userService.Deactivate(c.Request.Context(), ClaimsOf(c), id); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, deactivateView{Deactivated: true})
}

// 下面三个只有一个布尔字段的视图，本可以就地写成 gin.H。但 gin.H 是 map，
// 生成文档时拿不到任何字段信息，这几个接口的响应体就会在 OpenAPI 里变成空对象——
// 给它们一个类型，是让文档跟着代码走的最小代价。JSON 字段名与原 gin.H 的键一致。

// changePasswordView 改密结果。
type changePasswordView struct {
	Changed bool `json:"changed" example:"true"`
} // @name identity.ChangePasswordView

// resetPasswordView 重置口令结果。
type resetPasswordView struct {
	Reset bool `json:"reset" example:"true"`
} // @name identity.ResetPasswordView

// deactivateView 停用结果。
type deactivateView struct {
	Deactivated bool `json:"deactivated" example:"true"`
} // @name identity.DeactivateView

func parseUintParam(c *gin.Context, name string) (uint64, error) {
	raw := c.Param(name)
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, custom_errors.Invalid("参数 %s 必须是正整数: %s", name, raw)
	}
	return v, nil
}

// parsePage tolerates junk query values by falling back to defaults: a malformed
// page number is not worth failing a read request over.
func parsePage(c *gin.Context) shared_vo.Page {
	num, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("pageSize", "20"))
	return shared_vo.NewPage(num, size)
}
