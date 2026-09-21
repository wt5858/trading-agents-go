// Package http_handlers 把配置中心暴露成 HTTP 接口。
//
// 本包只做三件事：绑定并做形状校验、转调 domain_service、按统一信封渲染。
// 没有任何业务规则住在这里——它们属于 entities/ 与 domain_services/。
//
// 关于密钥：本包拿不到 SecretValue，也拿不到明文。
// 服务层返回的是已经脱敏的读模型（SanitizedProvider / SanitizedSetting），
// 所以「响应里不能出现明文密钥」这条要求在这里是结构性的，
// 不依赖任何一个 handler 作者记得调 Masked()。
// 唯一方向相反的是请求体：轮换密钥时明文从请求进来，立刻透传给服务层，
// 本包不留副本、不打日志。
package http_handlers

import (
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/system_config/domain_services"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
	"github.com/wt5858/trading-agents-go/internal/helpers/response"
)

// OperatorResolver 从请求上下文里取出调用者身份。
//
// 声明成注入的函数而不是直接读 identity 上下文的 Claims：配置中心不该在编译期
// 依赖身份上下文。怎么认证、Claims 存在哪个 key 里，是装配根的事。
type OperatorResolver func(c *gin.Context) (domain_services.Operator, bool)

type ConfigHandler struct {
	providerService *domain_services.LLMProviderService
	configService   *domain_services.ConfigService
	resolve         OperatorResolver
}

func NewConfigHandler(
	providerService *domain_services.LLMProviderService,
	configService *domain_services.ConfigService,
	resolve OperatorResolver,
) *ConfigHandler {
	return &ConfigHandler{
		providerService: providerService,
		configService:   configService,
		resolve:         resolve,
	}
}

// Register 挂载路由。
//
// 「探测全部」放在 /diagnostics/providers 而不是 /providers/test：
// 后者会和 /providers/:id 形成同层级的静态段与通配段冲突，
// 是路由树里最常见的一类隐患。换个前缀比赌路由库的行为可靠。
func (h *ConfigHandler) Register(rg *gin.RouterGroup, authRequired gin.HandlerFunc) {
	g := rg.Group("/config", authRequired)

	g.GET("/providers", h.ListProviders)
	g.POST("/providers", h.CreateProvider)
	g.GET("/providers/:id", h.GetProvider)
	g.PUT("/providers/:id", h.UpdateProvider)
	g.DELETE("/providers/:id", h.DeleteProvider)
	g.POST("/providers/:id/enable", h.EnableProvider)
	g.POST("/providers/:id/disable", h.DisableProvider)
	g.POST("/providers/:id/key", h.RotateKey)
	g.POST("/providers/:id/test", h.TestProvider)
	g.POST("/diagnostics/providers", h.TestAllProviders)

	g.GET("/settings", h.ListSettings)
	g.PUT("/settings", h.UpsertSetting)
	g.GET("/settings/:key", h.GetSetting)
	g.DELETE("/settings/:key", h.DeleteSetting)

	g.GET("/snapshot", h.Snapshot)
	g.POST("/reload", h.Reload)
}

// ---------------------------------------------------------------------------
// 供应商
// ---------------------------------------------------------------------------

// ListProviders 列出供应商配置。
//
// 这个接口有两种响应形状：默认分页返回全部配置，usable=true 时返回不分页的数组。
// OpenAPI 一个状态码只能声明一种形状，下面声明的是默认路径；
// 之所以不为此拆成两个接口，是因为调用方的心智是同一件事——「看供应商清单」。
//
// @Summary  列出模型供应商
// @Tags     系统配置
// @Produce  json
// @Security BearerAuth
// @Param    usable   query    bool false "为 true 时只返回当前真正能接活的供应商，且不分页"
// @Param    page     query    int  false "页码，从 1 开始，默认 1"
// @Param    pageSize query    int  false "每页条数，默认 20"
// @Success  200 {object} response.Envelope{data=response.PageData{items=[]domain_services.SanitizedProvider}} "apiKey 字段为掩码（首 3 位 + 末 4 位），未配置密钥时为空串"
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Failure  403 {object} response.Envelope "需要管理员权限"
// @Router   /config/providers [get]
func (h *ConfigHandler) ListProviders(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	page := parsePage(c)
	// usable=true 时只看当前真正能接活的几家，排障时比翻整张列表快。
	if strings.EqualFold(c.Query("usable"), "true") {
		providers, err := h.providerService.UsableProviders(c.Request.Context(), op)
		if err != nil {
			response.Fail(c, err)
			return
		}
		response.OK(c, providers)
		return
	}
	providers, total, err := h.providerService.List(c.Request.Context(), op, page)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OKPage(c, providers, total, page.Number, page.Size)
}

// createProviderRequest 注册一家供应商。
//
// apiKey 的 example 是一个明显的占位串，不是任何一把真密钥的形状：
// 文档会被截图、被复制进工单，示例值里放像样的密钥迟早会变成一次泄露。
type createProviderRequest struct {
	Name     string   `json:"name" binding:"required" example:"deepseek"`
	Kind     string   `json:"kind" binding:"required" enums:"openai_compat,anthropic,google" example:"openai_compat"`
	BaseURL  string   `json:"baseUrl" example:"https://api.deepseek.com"`
	APIKey   string   `json:"apiKey" example:"sk-****"`
	Models   []string `json:"models" example:"deepseek-chat,deepseek-reasoner"`
	Priority int      `json:"priority" example:"100"`
	Enabled  bool     `json:"enabled" example:"true"`
} // @name system.CreateProviderRequest

// CreateProvider 注册一家模型供应商。
//
// @Summary  注册模型供应商
// @Tags     系统配置
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    body body     createProviderRequest true "供应商配置；apiKey 为明文，响应里回的是掩码"
// @Success  200  {object} response.Envelope{data=domain_services.SanitizedProvider} "apiKey 字段为掩码，不回显刚提交的明文"
// @Failure  400  {object} response.Envelope "名称、协议类型、接入地址、优先级或模型名不合规"
// @Failure  401  {object} response.Envelope "未登录或令牌无效"
// @Failure  403  {object} response.Envelope "需要管理员权限"
// @Failure  409  {object} response.Envelope "同名供应商已存在"
// @Router   /config/providers [post]
func (h *ConfigHandler) CreateProvider(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	var req createProviderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, custom_errors.Invalid("请求参数不合法: %v", err))
		return
	}
	provider, err := h.providerService.Register(c.Request.Context(), op, domain_services.RegisterProviderCommand{
		Name:     req.Name,
		Kind:     req.Kind,
		BaseURL:  req.BaseURL,
		APIKey:   req.APIKey,
		Models:   req.Models,
		Priority: req.Priority,
		Enabled:  req.Enabled,
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, provider)
}

// GetProvider 取单个供应商配置。
//
// @Summary  查询模型供应商
// @Tags     系统配置
// @Produce  json
// @Security BearerAuth
// @Param    id  path     int true "供应商 ID"
// @Success  200 {object} response.Envelope{data=domain_services.SanitizedProvider} "apiKey 字段为掩码（首 3 位 + 末 4 位），未配置密钥时为空串"
// @Failure  400 {object} response.Envelope "供应商 ID 必须是正整数"
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Failure  403 {object} response.Envelope "需要管理员权限"
// @Failure  404 {object} response.Envelope "供应商不存在"
// @Router   /config/providers/{id} [get]
func (h *ConfigHandler) GetProvider(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	id, err := parseUintParam(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	provider, err := h.providerService.Get(c.Request.Context(), op, id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, provider)
}

// updateProviderRequest 用指针字段区分「没传这个字段」和「传了空值」。
// 不用指针的话，一次只想改优先级的请求会顺手把 baseUrl 清空。
//
// 这里没有 apiKey：密钥只能走独立的轮换接口。混在通用更新里，
// 管理台就很容易把 GET 回来的掩码原样 PUT 回去，
// 真密钥会被字符串 "sk-…abcd" 覆盖（值对象那一层也挡了一道）。
type updateProviderRequest struct {
	BaseURL  *string  `json:"baseUrl" example:"https://api.deepseek.com"`
	Models   []string `json:"models" example:"deepseek-chat,deepseek-reasoner"`
	Priority *int     `json:"priority" example:"100"`
} // @name system.UpdateProviderRequest

// UpdateProvider 改供应商的接入地址、模型列表或优先级。
//
// @Summary  更新模型供应商
// @Tags     系统配置
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path     int                   true  "供应商 ID"
// @Param    body body     updateProviderRequest true  "只传要改的字段；字段缺省表示不动，不表示清空。密钥不在这里改，走 /key"
// @Success  200  {object} response.Envelope{data=domain_services.SanitizedProvider} "apiKey 字段为掩码"
// @Failure  400  {object} response.Envelope "供应商 ID 非法，或接入地址、模型列表、优先级不合规"
// @Failure  401  {object} response.Envelope "未登录或令牌无效"
// @Failure  403  {object} response.Envelope "需要管理员权限"
// @Failure  404  {object} response.Envelope "供应商不存在"
// @Router   /config/providers/{id} [put]
func (h *ConfigHandler) UpdateProvider(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	id, err := parseUintParam(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	var req updateProviderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, custom_errors.Invalid("请求参数不合法: %v", err))
		return
	}
	provider, err := h.providerService.Update(c.Request.Context(), op, id, domain_services.UpdateProviderCommand{
		BaseURL:  req.BaseURL,
		Models:   req.Models,
		Priority: req.Priority,
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, provider)
}

// DeleteProvider 删除供应商配置。
//
// @Summary  删除模型供应商
// @Tags     系统配置
// @Produce  json
// @Security BearerAuth
// @Param    id  path     int true "供应商 ID"
// @Success  200 {object} response.Envelope{data=deletedView}
// @Failure  400 {object} response.Envelope "供应商 ID 必须是正整数"
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Failure  403 {object} response.Envelope "需要管理员权限"
// @Failure  404 {object} response.Envelope "供应商不存在"
// @Router   /config/providers/{id} [delete]
func (h *ConfigHandler) DeleteProvider(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	id, err := parseUintParam(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	if err := h.providerService.Delete(c.Request.Context(), op, id); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, deletedView{Deleted: true})
}

// EnableProvider 启用供应商。
//
// @Summary  启用模型供应商
// @Tags     系统配置
// @Produce  json
// @Security BearerAuth
// @Param    id  path     int true "供应商 ID"
// @Success  200 {object} response.Envelope{data=domain_services.SanitizedProvider} "usable 字段表示这家现在是否真的能接活（启用 + 有密钥 + 有模型）"
// @Failure  400 {object} response.Envelope "供应商 ID 必须是正整数"
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Failure  403 {object} response.Envelope "需要管理员权限"
// @Failure  404 {object} response.Envelope "供应商不存在"
// @Failure  409 {object} response.Envelope "供应商已处于启用状态"
// @Router   /config/providers/{id}/enable [post]
func (h *ConfigHandler) EnableProvider(c *gin.Context) {
	h.switchProvider(c, true)
}

// DisableProvider 停用供应商。
//
// @Summary  停用模型供应商
// @Tags     系统配置
// @Produce  json
// @Security BearerAuth
// @Param    id  path     int true "供应商 ID"
// @Success  200 {object} response.Envelope{data=domain_services.SanitizedProvider}
// @Failure  400 {object} response.Envelope "供应商 ID 必须是正整数"
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Failure  403 {object} response.Envelope "需要管理员权限"
// @Failure  404 {object} response.Envelope "供应商不存在"
// @Failure  409 {object} response.Envelope "供应商已处于停用状态"
// @Router   /config/providers/{id}/disable [post]
func (h *ConfigHandler) DisableProvider(c *gin.Context) {
	h.switchProvider(c, false)
}

func (h *ConfigHandler) switchProvider(c *gin.Context, enable bool) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	id, err := parseUintParam(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	var provider *domain_services.SanitizedProvider
	if enable {
		provider, err = h.providerService.Enable(c.Request.Context(), op, id)
	} else {
		provider, err = h.providerService.Disable(c.Request.Context(), op, id)
	}
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, provider)
}

// rotateKeyRequest 轮换密钥的请求体。
//
// example 用占位串而不是任何像样的密钥形状，理由同 createProviderRequest。
type rotateKeyRequest struct {
	APIKey string `json:"apiKey" binding:"required" example:"sk-****"`
} // @name system.RotateKeyRequest

// RotateKey 是全包唯一接触密钥明文的入口。
// 明文只在这个函数的栈上存在几行，随即交给服务层转成 SecretValue；
// 它不会被日志记录，也不会出现在响应里（响应回的是掩码）。
//
// @Summary  轮换供应商密钥
// @Tags     系统配置
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path     int              true "供应商 ID"
// @Param    body body     rotateKeyRequest true "新密钥明文；不接受掩码形态（如 sk-…abcd），那通常是把查询结果原样回传导致的误覆盖"
// @Success  200  {object} response.Envelope{data=domain_services.SanitizedProvider} "apiKey 字段是新密钥的掩码，接口不回显明文"
// @Failure  400  {object} response.Envelope "供应商 ID 非法，或 apiKey 为空、为掩码形态"
// @Failure  401  {object} response.Envelope "未登录或令牌无效"
// @Failure  403  {object} response.Envelope "需要管理员权限"
// @Failure  404  {object} response.Envelope "供应商不存在"
// @Router   /config/providers/{id}/key [post]
func (h *ConfigHandler) RotateKey(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	id, err := parseUintParam(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	var req rotateKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		// 注意这里不回显请求体：绑定失败的报文里装的就是密钥。
		response.Fail(c, custom_errors.Invalid("请求参数不合法：apiKey 不能为空"))
		return
	}
	provider, err := h.providerService.RotateKey(c.Request.Context(), op, id, req.APIKey)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, provider)
}

// TestProvider 对单家供应商发一次真实探测。
//
// 探测不通不是接口失败：结论在 data.ok / data.message 里，HTTP 仍然是 200。
// 503 只留给「压根没法探测」——装配根没有注入探测器。
//
// @Summary  探测单家供应商连通性
// @Tags     系统配置
// @Produce  json
// @Security BearerAuth
// @Param    id  path     int true "供应商 ID"
// @Success  200 {object} response.Envelope{data=domain_services.ProbeResult} "探测失败同样是 200，看 data.ok；message 只含领域消息，不含厂商原始报文"
// @Failure  400 {object} response.Envelope "供应商 ID 必须是正整数"
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Failure  403 {object} response.Envelope "需要管理员权限"
// @Failure  404 {object} response.Envelope "供应商不存在"
// @Failure  503 {object} response.Envelope "未配置连通性探测器"
// @Router   /config/providers/{id}/test [post]
func (h *ConfigHandler) TestProvider(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	id, err := parseUintParam(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	result, err := h.providerService.TestConnection(c.Request.Context(), op, id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, result)
}

// TestAllProviders 批量探测。单家探测失败不是接口失败，
// 结果里带着每一家的结论，所以照常返回 200。
//
// @Summary  探测全部启用中供应商的连通性
// @Tags     系统配置
// @Produce  json
// @Security BearerAuth
// @Success  200 {object} response.Envelope{data=[]domain_services.ProbeResult} "逐家一条结论；全都不通也是 200，没有启用中的供应商时是空数组"
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Failure  403 {object} response.Envelope "需要管理员权限"
// @Failure  503 {object} response.Envelope "未配置连通性探测器，或探测过程被中断"
// @Router   /config/diagnostics/providers [post]
func (h *ConfigHandler) TestAllProviders(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	results, err := h.providerService.TestAllConnections(c.Request.Context(), op)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, results)
}

// ---------------------------------------------------------------------------
// 配置项
// ---------------------------------------------------------------------------

// ListSettings 按配置域列出配置项。
//
// @Summary  按域列出配置项
// @Tags     系统配置
// @Produce  json
// @Security BearerAuth
// @Param    scope query    string true "配置域" Enums(llm, market, sync, feature)
// @Success  200   {object} response.Envelope{data=[]domain_services.SanitizedSetting} "secret=true 的项，value 已按密钥规则打码"
// @Failure  400   {object} response.Envelope "未指定配置域，或配置域取值非法"
// @Failure  401   {object} response.Envelope "未登录或令牌无效"
// @Failure  403   {object} response.Envelope "需要管理员权限"
// @Router   /config/settings [get]
func (h *ConfigHandler) ListSettings(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	scope := strings.TrimSpace(c.Query("scope"))
	if scope == "" {
		response.Fail(c, custom_errors.Invalid("必须指定配置域 scope（llm / market / sync / feature）"))
		return
	}
	settings, err := h.configService.ListByScope(c.Request.Context(), op, scope)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, settings)
}

// GetSetting 取单项配置。
//
// @Summary  查询单项配置
// @Tags     系统配置
// @Produce  json
// @Security BearerAuth
// @Param    key path     string true "配置键，2-4 段点分格式，首段是配置域" example(llm.default_model)
// @Success  200 {object} response.Envelope{data=domain_services.SanitizedSetting} "密钥类配置项（键以 _key/_token/_secret/_password/_credential 结尾）的 value 已打码"
// @Failure  400 {object} response.Envelope "配置键格式非法"
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Failure  403 {object} response.Envelope "需要管理员权限"
// @Failure  404 {object} response.Envelope "配置项不存在"
// @Router   /config/settings/{key} [get]
func (h *ConfigHandler) GetSetting(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	setting, err := h.configService.Get(c.Request.Context(), op, c.Param("key"))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, setting)
}

type upsertSettingRequest struct {
	Key         string `json:"key" binding:"required" example:"llm.default_model"`
	Value       string `json:"value" example:"deepseek-chat"`
	Description string `json:"description" example:"默认分析模型"`
} // @name system.UpsertSettingRequest

// UpsertSetting 走 PUT /settings 而不是 PUT /settings/:key：
// 配置键本身带点（llm.default_model），放进路径参数会和某些网关的
// 路径归一化规则打架，放在请求体里没有这个问题。
//
// @Summary  写入配置项
// @Tags     系统配置
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    body body     upsertSettingRequest true "配置键与值；键不存在则新建，存在则改值"
// @Success  200  {object} response.Envelope{data=domain_services.SanitizedSetting} "密钥类配置项回的是打码后的 value"
// @Failure  400  {object} response.Envelope "配置键格式非法，或配置说明超长"
// @Failure  401  {object} response.Envelope "未登录或令牌无效"
// @Failure  403  {object} response.Envelope "需要管理员权限"
// @Router   /config/settings [put]
func (h *ConfigHandler) UpsertSetting(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	var req upsertSettingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, custom_errors.Invalid("请求参数不合法: %v", err))
		return
	}
	setting, err := h.configService.Set(c.Request.Context(), op, domain_services.SetSettingCommand{
		Key:         req.Key,
		Value:       req.Value,
		Description: req.Description,
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, setting)
}

// DeleteSetting 删除单项配置，删除后该项回落到文件/环境变量里的默认值。
//
// @Summary  删除配置项
// @Tags     系统配置
// @Produce  json
// @Security BearerAuth
// @Param    key path     string true "配置键" example(llm.default_model)
// @Success  200 {object} response.Envelope{data=deletedView}
// @Failure  400 {object} response.Envelope "配置键格式非法"
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Failure  403 {object} response.Envelope "需要管理员权限"
// @Failure  404 {object} response.Envelope "配置项不存在"
// @Router   /config/settings/{key} [delete]
func (h *ConfigHandler) DeleteSetting(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	if err := h.configService.Delete(c.Request.Context(), op, c.Param("key")); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, deletedView{Deleted: true})
}

// ---------------------------------------------------------------------------
// 快照与重载
// ---------------------------------------------------------------------------

// Snapshot 取管理台用的全局配置快照。
//
// 快照只含数据库管的那一半配置——数据库连接、端口、JWT 密钥这些基础设施参数
// 不在其中，也不会加进来。
//
// @Summary  查询全局配置快照
// @Tags     系统配置
// @Produce  json
// @Security BearerAuth
// @Success  200 {object} response.Envelope{data=domain_services.ConfigSnapshot} "供应商密钥与密钥类配置项均为掩码；providers 只含启用中的供应商"
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Failure  403 {object} response.Envelope "需要管理员权限"
// @Router   /config/snapshot [get]
func (h *ConfigHandler) Snapshot(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	snapshot, err := h.configService.Snapshot(c.Request.Context(), op)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, snapshot)
}

type reloadRequest struct {
	Scope  string `json:"scope" binding:"required" enums:"llm,market,sync,feature" example:"llm"`
	Reason string `json:"reason" example:"切换默认模型后让各消费方重读"`
} // @name system.ReloadRequest

// Reload 广播「某个域的配置该重读了」。
//
// 200 只代表事件已发出，不代表各消费方已经重读完——配置中心不知道谁缓存了什么，
// 也就给不出「全部生效」这个结论。
//
// @Summary  请求重载配置
// @Tags     系统配置
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    body body     reloadRequest true "要重载的配置域与原因"
// @Success  200  {object} response.Envelope{data=reloadRequestedView} "只表示重载事件已广播"
// @Failure  400  {object} response.Envelope "配置域取值非法"
// @Failure  401  {object} response.Envelope "未登录或令牌无效"
// @Failure  403  {object} response.Envelope "需要管理员权限"
// @Router   /config/reload [post]
func (h *ConfigHandler) Reload(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	var req reloadRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, custom_errors.Invalid("请求参数不合法: %v", err))
		return
	}
	if err := h.configService.Reload(c.Request.Context(), op, req.Scope, req.Reason); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, reloadRequestedView{Requested: true})
}

// ---------------------------------------------------------------------------
// 响应视图
//
// 下面两个结构体本可以写成 gin.H，但 gin.H 是 map[string]any，在 OpenAPI 里
// 只能落成一个无字段的空对象——响应形状一旦没有类型，文档就只能靠人手写，
// 而手写的那份迟早和代码分家。字段名与原来的 gin.H 键逐字一致。
// ---------------------------------------------------------------------------

// deletedView 删除结果，供应商与配置项的删除接口共用。
type deletedView struct {
	Deleted bool `json:"deleted" example:"true"`
} // @name system.DeletedView

// reloadRequestedView 重载请求已受理。
// 名字里是 requested 而不是 reloaded：这里只保证事件发出去了。
type reloadRequestedView struct {
	Requested bool `json:"requested" example:"true"`
} // @name system.ReloadRequestedView

// ---------------------------------------------------------------------------
// 公共
// ---------------------------------------------------------------------------

// operator 取调用者身份，取不到时已经写好响应，调用方直接 return 即可。
func (h *ConfigHandler) operator(c *gin.Context) (domain_services.Operator, bool) {
	if h.resolve == nil {
		response.Fail(c, custom_errors.Unauthorized("未登录"))
		return domain_services.Operator{}, false
	}
	op, ok := h.resolve(c)
	if !ok {
		response.Fail(c, custom_errors.Unauthorized("未登录"))
		return domain_services.Operator{}, false
	}
	return op, true
}

func parseUintParam(c *gin.Context, name string) (uint64, error) {
	raw := c.Param(name)
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, custom_errors.Invalid("参数 %s 必须是正整数: %s", name, raw)
	}
	return v, nil
}

// parsePage 对垃圾查询参数一律回落到默认值：
// 一个写坏的页码不值得让一次读请求失败。
func parsePage(c *gin.Context) shared_vo.Page {
	num, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("pageSize", "20"))
	return shared_vo.NewPage(num, size)
}
