// Package domain_services 编排配置中心的业务用例。
//
// # 分层职责
//
//   - 业务不变式属于 entities/（Usable() 那套判定），本层不重复判定；
//   - 事务属于 repositories/，本层不持有任何事务句柄；
//   - application/ 只放 handler，所有编排都在这里。
//
// 本层同时声明它消费的外部端口（ProviderProbe）。按 Go 惯例由消费方声明接口：
// 本包只认这个签名，实现住在 internal/helpers/llm，由装配根注入。
// 反过来 import helpers/llm 会让领域层依赖一个具体的 HTTP 客户端实现。
//
// # 配置分层规则（本上下文最重要的设计约束）
//
// 这套系统有两个配置来源，边界是硬的：
//
//	文件 / 环境变量（config/config.go）—— 基础设施的唯一真相来源
//	    MySQL / Redis / Mongo 连接参数、HTTP 端口、JWT 密钥、bcrypt cost。
//	    这些【绝不进数据库】。理由不是洁癖：配置表本身就存在 MySQL 里，
//	    把 mysql.password 做成一行配置，意味着写坏这一行就再也连不上存着它的库，
//	    服务把自己锁在了自己的数据库外面。启动参数必须在启动之前就能拿到。
//
//	数据库（本上下文）—— 运行期可调参数的唯一真相来源
//	    LLM 供应商（接入地址、密钥、模型列表、启停）、行情数据源开关与 token、
//	    功能开关、同步任务调优参数。这些的共同点是：改了之后不需要重启进程，
//	    改错了最多让某个功能降级，不会让服务起不来。
//
// 启动顺序是「文件打底，数据库覆盖」：先用文件/环境变量构造出一份完整的可用配置，
// 再用 SystemSettingRepository.LoadAll 的结果逐项覆盖运行期那部分。
// 覆盖方向只有这一个——数据库里没有的项就用文件值，不存在「数据库说了算但库里没有」
// 这种会让系统起不来的情况。
//
// 所以：任何人想往 system_settings 表里加一行 mysql.password / redis.addr /
// auth.jwt_secret，答案都是不行。SettingScope 只有 llm / market / sync / feature
// 四个取值，正是为了让这件事在类型层面就做不到。
package domain_services

import (
	"context"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/system_config/entities"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// Operator 是发起调用的身份。
//
// 刻意不复用 identity 上下文的 Claims：那会让配置中心在编译期依赖身份上下文。
// 两个限界上下文之间只传数据，不共享类型；怎么认证是装配根的事。
type Operator struct {
	UserID  uint64
	IsAdmin bool
}

// requireAdmin 是本上下文所有写操作的前置条件。
//
// 配置中心的每一次修改都能影响全系统——停用一家供应商会让所有分析任务失去一条链路，
// 改一个开关会改变所有人的行为。这里没有「自己改自己的」这种较松的场景，
// 所以不区分操作对象，一律要求管理员。
//
// 读操作同样要求管理员：即便密钥已经掩码，供应商清单本身也会暴露
// 「这套系统接了哪些厂商、走的什么网关」，那是内部拓扑信息。
func requireAdmin(op Operator) error {
	if op.UserID == 0 {
		return custom_errors.Unauthorized("未登录")
	}
	if !op.IsAdmin {
		return custom_errors.Forbidden("需要管理员权限")
	}
	return nil
}

// ResolvedProvider 是一份【含明文密钥】的供应商快照。
//
// 全系统只有两个地方会产出它：ProviderResolver.Resolve（装配路由）和
// LLMProviderService.TestConnection（连通性探测）。两者都经过下面唯一的 expose()。
//
// APIKey 打了 json:"-"，String() 也做了脱敏：这个结构体本来就不该被序列化或打印，
// 但「不该」不是保障，所以即便有人真的把它塞进响应或日志，泄露的也只是掩码。
type ResolvedProvider struct {
	ID       uint64
	Name     string
	Kind     string
	BaseURL  string
	APIKey   string `json:"-"`
	Models   []string
	Priority int
}

// String 保证 %v / %s 打不出明文。注意 %+v 仍会逐字段展开 APIKey——
// 这正是这个结构体必须留在装配路径上、不许四处传递的原因。
func (p ResolvedProvider) String() string {
	return "ResolvedProvider(" + p.Name + "/" + p.Kind + ", key=****)"
}

// expose 是全上下文唯一把密钥明文取出来的函数。
//
// 单独拎成一个函数、而不是让各处自己调 APIKey.Expose()，是为了让「密钥在这里
// 离开了保护」这件事成为一个可审计的点：grep 一下 Expose( 就能穷举全部泄露面
// （另一处在 dtos，那是写库，无可避免）。
// 这也是为什么 ProviderResolver 是一个独立的窄接口，而不是在通用查询 API 上
// 加一个 includeSecret 参数——那样的话每个调用点都成了潜在的泄露点。
func expose(p *entities.LLMProviderConfig) ResolvedProvider {
	return ResolvedProvider{
		ID:       p.ID,
		Name:     p.Name.String(),
		Kind:     p.Kind.String(),
		BaseURL:  p.BaseURL.String(),
		APIKey:   p.APIKey.Expose(),
		Models:   append([]string(nil), p.Models...),
		Priority: p.Priority,
	}
}

// ProviderProbe 是连通性探测端口，由 internal/helpers/llm 侧实现。
//
// 声明成「给一份已解析的供应商 + 一个模型名，回一个错误」这样的窄签名，
// 而不是把整个 Router 交进来：探测只需要发一次最小请求确认密钥和地址是通的。
// 给它 Router 就等于给了它改路由表的能力，而那是装配根的职责。
type ProviderProbe interface {
	Probe(ctx context.Context, target ResolvedProvider, model string) error
}

// ProbeResult 是一次探测的结论。
//
// 探测失败不是本次用例的失败：管理员点「测试全部」，期望看到的是一张
// 「哪几家通、哪几家不通」的表，而不是在第一家不通时就中断。
// 所以失败信息作为数据返回，而不是作为 error 抛出。
type ProbeResult struct {
	ProviderID   uint64 `json:"providerId"`
	ProviderName string `json:"providerName"`
	Model        string `json:"model"`
	OK           bool   `json:"ok"`
	Message      string `json:"message"`
}
