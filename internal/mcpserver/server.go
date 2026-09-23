package mcpserver

import (
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	identity_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/identity/domain_services"
)

// serverName / serverVersion 是 MCP 握手时报给客户端的身份。
const (
	serverName    = "trading-agents"
	serverVersion = "0.1.0"
)

// ToolRegistrar 是一组工具的注册者，由各上下文的 application/mcp_tools 实现。
//
// 按 Go 惯例由消费方声明接口。实现方不需要 import 本包——Go 的结构化类型
// 让「满足接口」不产生依赖，这正是本包能够完全不认识 analysis、watchlist、
// screening 的原因。新增一个上下文的工具集，本文件一行都不用改。
type ToolRegistrar interface {
	RegisterTools(srv *mcp.Server)
}

// Server 是挂在 /mcp 上的 MCP 工具服务器。
//
// 它是一个具体类型而不是裸的 http.Handler：Wire 按类型装配，
// 一个返回接口的 provider 会和任何别的 http.Handler provider 撞车，
// 而那种冲突的报错信息指向的是生成代码，极难读。
type Server struct {
	handler http.Handler
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// New 组装 MCP 服务器。
//
// # 为什么每个会话共用同一个 *mcp.Server
//
// getServer 每来一个新会话调一次，而这里固定返回同一个实例。
// 工具集合是编译期定死的，不随调用者变化；真正随调用者变的是身份，
// 而身份走的是 context（见 auth.go），不是服务器实例。
// 每个会话造一个新 Server 只会把十几个工具的 schema 推断重跑一遍。
//
// # 鉴权包在最外层
//
// 顺序是 authMiddleware(streamableHandler)：未登录的请求连 MCP 协议解析
// 都不该进入。反过来包的话，tools/list 这种不带业务语义的方法会在鉴权之前
// 就把全部工具名与参数 schema 吐出去。
func New(auth *identity_services.AuthService, registrars []ToolRegistrar, log *zap.Logger) *Server {
	if log == nil {
		log = zap.NewNop()
	}

	impl := &mcp.Implementation{Name: serverName, Version: serverVersion}
	srv := mcp.NewServer(impl, nil)

	for _, r := range registrars {
		if r == nil {
			continue
		}
		r.RegisterTools(srv)
	}

	streamable := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		nil,
	)

	log.Info("MCP 工具服务器已装配",
		zap.String("endpoint", "/mcp"),
		zap.Int("registrars", len(registrars)))
	return &Server{handler: authMiddleware(streamable, auth)}
}
