package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	swagger_files "github.com/swaggo/files"
	gin_swagger "github.com/swaggo/gin-swagger"
	"go.uber.org/zap"

	"github.com/wt5858/trading-agents-go/config"
	agent_handlers "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/application/http_handlers"
	analysis_handlers "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/application/http_handlers"
	identity_handlers "github.com/wt5858/trading-agents-go/internal/bounded_contexts/identity/application/http_handlers"
	identity_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/identity/domain_services"
	notification_handlers "github.com/wt5858/trading-agents-go/internal/bounded_contexts/notification/application/http_handlers"
	paper_handlers "github.com/wt5858/trading-agents-go/internal/bounded_contexts/paper_trading/application/http_handlers"
	report_handlers "github.com/wt5858/trading-agents-go/internal/bounded_contexts/report/application/http_handlers"
	scheduling_handlers "github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/application/http_handlers"
	screening_handlers "github.com/wt5858/trading-agents-go/internal/bounded_contexts/screening/application/http_handlers"
	stock_handlers "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/application/http_handlers"
	system_handlers "github.com/wt5858/trading-agents-go/internal/bounded_contexts/system_config/application/http_handlers"
	watchlist_handlers "github.com/wt5858/trading-agents-go/internal/bounded_contexts/watchlist/application/http_handlers"
	"github.com/wt5858/trading-agents-go/internal/helpers/response"
	"github.com/wt5858/trading-agents-go/internal/mcpserver"

	// docs 是 `make swagger` 生成的包，只在 init 里把 OpenAPI 文档注册进 swag 的全局
	// 注册表——除此之外没有任何导出符号，所以只能空导入。没有这一行，/swagger 会起来，
	// 但打开是一个「Failed to load API definition」的空壳。
	_ "github.com/wt5858/trading-agents-go/docs"
)

type Server struct {
	http *http.Server
	log  *zap.Logger
}

// Handlers 是路由层需要的全部 HTTP 处理器。
//
// 它没有任何行为，只是一份清单——存在的理由是让「加了一个上下文却忘了注册路由」
// 变成一个编译期问题：新处理器必须在这里加一个字段，而 Wire 会因为
// 找不到它的 provider 而拒绝生成代码。
//
// 换成 New 收十三个参数也能达到同样效果，但那个参数表没人愿意维护。
type Handlers struct {
	Auth         *identity_handlers.AuthHandler
	User         *identity_handlers.UserHandler
	Stock        *stock_handlers.StockHandler
	Sync         *stock_handlers.SyncHandler
	Agent        *agent_handlers.AgentHandler
	Analysis     *analysis_handlers.AnalysisHandler
	Report       *report_handlers.ReportHandler
	ScheduledJob *scheduling_handlers.ScheduledJobHandler
	Config       *system_handlers.ConfigHandler
	Watchlist    *watchlist_handlers.WatchlistHandler
	PaperTrading *paper_handlers.PaperTradingHandler
	Notification *notification_handlers.NotificationHandler
	Screening    *screening_handlers.ScreeningHandler
}

// New 组装路由并返回可启动的 HTTP 服务。
//
// 它收 *config.Config 与各处理器，而不再收整个装配容器：本包因此不依赖
// internal/di，依赖方向是单向的（di -> server）。反过来会形成 import 环，
// 也会让「路由层到底用了容器里的什么」永远说不清楚。
func New(
	cfg *config.Config,
	log *zap.Logger,
	authService *identity_services.AuthService,
	mcp *mcpserver.Server,
	h *Handlers,
) *Server {
	if cfg.App.IsProd() {
		gin.SetMode(gin.ReleaseMode)
	}

	engine := gin.New()
	engine.Use(RequestID(), Logger(log), Recovery(log), CORS(cfg.HTTP.AllowedOrigins))

	// 未匹配路由同样走统一信封，免得前端在 404 时拿到 gin 的裸文本。
	engine.NoRoute(func(ctx *gin.Context) {
		response.FailWith(ctx, response.CodeNotFound, http.StatusNotFound, "接口不存在")
	})

	engine.GET("/healthz", func(ctx *gin.Context) {
		response.OK(ctx, gin.H{"status": "ok", "app": cfg.App.Name})
	})

	// Swagger UI 只在非生产环境挂载。
	//
	// 它不是鉴权问题——文档本身不含密钥。问题在于这份文档会把全部接口、参数名与
	// 取值范围一次性摊开，等于给线上环境附赠一张攻击面地图；而生产环境的调用方
	// 应该照着发布出去的 swagger.json 对接，不是照着线上实例现挂的那一份。
	if !cfg.App.IsProd() {
		engine.GET("/swagger/*any", gin_swagger.WrapHandler(swagger_files.Handler))
		log.Info("Swagger UI 已挂载", zap.String("url", "http://"+cfg.HTTP.Addr()+"/swagger/index.html"))
	}

	// MCP 挂在 /api/v1 之外，且不套 authRequired。
	//
	// 两条都是刻意的。挂在 /api/v1 之外：那个前缀下的每条路由都遵循本项目的
	// 统一响应信封（response.Envelope），而 MCP 说的是 JSON-RPC，
	// 把它塞进同一个前缀会让「/api/v1 下的响应长什么样」这条约定出现唯一的例外。
	// 不套 authRequired：那是个 gin 中间件，它把 Claims 写进 *gin.Context，
	// 而 MCP 处理器手上只有 context.Context，取不到；鉴权由 mcpserver 自己那层
	// 完成（internal/mcpserver/auth.go），走的是同一个 AuthService。
	// 单个端点、放行全部方法：Streamable HTTP 传输在同一个 URL 上用
	// POST 发消息、GET 开 SSE 流、DELETE 关会话，没有子路径。
	// 不要顺手再注册一条 /mcp/*any——它和这条在 gin 的路由树里是冲突的，
	// 表现为进程启动时直接 panic。
	engine.Any("/mcp", gin.WrapH(mcp))

	authRequired := AuthRequired(authService)
	api := engine.Group("/api/v1")

	h.Auth.Register(api, authRequired)
	h.User.Register(api, authRequired)
	h.Stock.Register(api)
	h.Agent.Register(api)
	h.Analysis.Register(api, authRequired)
	h.Report.Register(api, authRequired)
	h.Sync.Register(api, authRequired)
	h.ScheduledJob.Register(api, authRequired)
	h.Config.Register(api, authRequired)
	h.Watchlist.Register(api, authRequired)
	h.PaperTrading.Register(api, authRequired)
	h.Notification.Register(api, authRequired)
	h.Screening.Register(api, authRequired)

	return &Server{
		http: &http.Server{
			Addr:         cfg.HTTP.Addr(),
			Handler:      engine,
			ReadTimeout:  cfg.HTTP.ReadTimeout,
			WriteTimeout: cfg.HTTP.WriteTimeout,
		},
		log: log,
	}
}

func (s *Server) Start() error {
	s.log.Info("HTTP 服务启动", zap.String("addr", s.http.Addr))
	if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown 优雅停机：停止接收新请求，给在途请求留出完成时间。
func (s *Server) Shutdown(ctx context.Context) error {
	shutdownCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return s.http.Shutdown(shutdownCtx)
}
