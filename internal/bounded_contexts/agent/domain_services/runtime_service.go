package domain_services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/repositories"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/concurrency"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// RuntimeConfig 是运行时的兜底参数，契约里没指定时生效。
type RuntimeConfig struct {
	Temperature   float64
	MaxTokens     int
	MaxToolRounds int
	// ToolFanOutLimit 是同一轮里并行执行工具的上限。
	ToolFanOutLimit int
	// MaxToolResultRunes 是单个工具结果允许占用的字符数上限。
	MaxToolResultRunes int
}

// 运行时默认值。
const (
	defaultTemperature   = 0.4
	defaultMaxTokens     = 2400
	defaultMaxToolRounds = 3

	// defaultToolFanOut 是同一轮工具调用的并发上限。
	//
	// 取 4 而不是「有几个调几个」：模型一轮最多能发起十几个工具调用，
	// 而每个工具背后都是一次 Mongo 查询。上层已经有三到六位分析师在并行，
	// 两层扇出相乘才是真正的并发数——4 × 3 = 12 个在途查询是数据库能稳住的量级，
	// 不设限则是 12 × N，一次批量分析足以打满连接池。
	defaultToolFanOut = 4

	// defaultMaxToolResultRunes 是单个工具结果的字符上限。
	//
	// 工具结果会被完整回灌进下一轮请求，而且会在后续每一轮里重复出现。
	// 一份没有上限的 K 线数据可以轻易吃掉几万 token，撞上上下文长度上限的表现
	// 是模型在最后一步突然失败，而那时这次分析已经花掉了大部分成本。
	defaultMaxToolResultRunes = 4000
)

func (c RuntimeConfig) normalized() RuntimeConfig {
	if c.Temperature <= 0 {
		c.Temperature = defaultTemperature
	}
	if c.MaxTokens <= 0 {
		c.MaxTokens = defaultMaxTokens
	}
	if c.MaxToolRounds <= 0 {
		c.MaxToolRounds = defaultMaxToolRounds
	}
	if c.ToolFanOutLimit <= 0 {
		c.ToolFanOutLimit = defaultToolFanOut
	}
	if c.MaxToolResultRunes <= 0 {
		c.MaxToolResultRunes = defaultMaxToolResultRunes
	}
	return c
}

// RuntimeService 实现 entities.Runtime：渲染提示词、调模型、驱动工具调用循环。
//
// 它是整个上下文里唯一知道「模型是通过 HTTP 调用的」这件事的地方
// （而且也只是间接知道——它只认 LLMClient 端口）。
// 实体层的十四位成员通过 Runtime 接口使唤它，谁也不认识 OpenAI 或 Anthropic。
type RuntimeService struct {
	router  ModelRouter
	tools   ToolRegistry
	prompts *PromptService
	cache   *repositories.CompletionCache
	cfg     RuntimeConfig
	log     *zap.Logger
}

var _ entities.Runtime = (*RuntimeService)(nil)

func NewRuntimeService(
	router ModelRouter,
	tools ToolRegistry,
	prompts *PromptService,
	cache *repositories.CompletionCache,
	cfg RuntimeConfig,
	log *zap.Logger,
) *RuntimeService {
	if prompts == nil {
		prompts = NewPromptService()
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &RuntimeService{
		router: router, tools: tools, prompts: prompts,
		cache: cache, cfg: cfg.normalized(), log: log,
	}
}

// Execute 让一位成员完成一次发言。
func (s *RuntimeService) Execute(ctx context.Context, turn entities.Turn) (entities.TurnResult, error) {
	if s.router == nil {
		return entities.TurnResult{}, custom_errors.Internal("未配置模型路由")
	}

	messages, err := s.prompts.Render(turn)
	if err != nil {
		return entities.TurnResult{}, err
	}

	contract := turn.Contract
	req := value_objects.ChatRequest{
		Model:         turn.Snapshot.Model,
		Messages:      messages,
		Tools:         s.specsFor(contract.Access),
		Temperature:   pickFloat(contract.Temperature, s.cfg.Temperature),
		MaxTokens:     pickInt(contract.MaxTokens, s.cfg.MaxTokens),
		MaxToolRounds: pickToolRounds(contract, s.cfg.MaxToolRounds),
		Access:        contract.Access,
	}

	promptChars, promptDigest := digestRequest(req)

	// 路由在这里解析一次，解析结果直接交给 chat。
	//
	// 必须在调模型之前拿到真实模型名：缓存键是「模型 + 提示词指纹」，
	// 而请求里的 Model 可以是空串（走默认）或一个别名——拿它当键，
	// 换了默认模型之后会命中上一个模型产出的结论，且看不出任何异常。
	client, model, err := s.router.Resolve(req.Model)
	if err != nil {
		return entities.TurnResult{PromptChars: promptChars, PromptDigest: promptDigest}, err
	}

	if cached, ok := s.lookupCache(ctx, model, promptDigest); ok {
		s.log.Debug("命中发言缓存",
			zap.String("agent", contract.Kind.String()),
			zap.String("model", model),
			zap.String("prompt_digest", promptDigest))
		return entities.TurnResult{
			Content:    cached.Content,
			ToolRounds: cached.ToolRounds,
			Truncated:  cached.Truncated,
			Model:      model,
			// Usage 留零值：这次运行一个字都没发给模型，记上消耗会让成本统计虚高。
			PromptChars:  promptChars,
			PromptDigest: promptDigest,
			CacheHit:     true,
		}, nil
	}

	resp, err := s.chat(ctx, client, model, req, ToolInvocation{Code: turn.Snapshot.Code, TradeDate: turn.Snapshot.TradeDate})
	if err != nil {
		// 失败也把已经产生的消耗与提示词摘要带回去：撞上下文上限之前的那几轮是真花了钱的，
		// 而「它到底看到了多长的输入」正是这类失败的第一个排查问题。
		return entities.TurnResult{
			Usage:        resp.Usage,
			Model:        resp.Model,
			PromptChars:  promptChars,
			PromptDigest: promptDigest,
		}, err
	}
	content := strings.TrimSpace(resp.Content)
	s.storeCache(ctx, model, promptDigest, value_objects.CachedTurn{
		Content:    content,
		ToolRounds: resp.ToolRounds,
		Truncated:  resp.Truncated,
		Model:      resp.Model,
	})

	return entities.TurnResult{
		Content:      content,
		Usage:        resp.Usage,
		ToolRounds:   resp.ToolRounds,
		Truncated:    resp.Truncated,
		Model:        resp.Model,
		PromptChars:  promptChars,
		PromptDigest: promptDigest,
	}, nil
}

// lookupCache / storeCache 把「有没有配缓存」这个判断收在一处。
//
// 缓存是可选依赖：注入 nil 表示不启用，此时全部调用照常打到模型上。
// 让每个调用点各写一次 if s.cache != nil，迟早有一处漏掉而 panic。
func (s *RuntimeService) lookupCache(ctx context.Context, model, digest string) (value_objects.CachedTurn, bool) {
	if s.cache == nil {
		return value_objects.CachedTurn{}, false
	}
	return s.cache.Get(ctx, model, digest)
}

func (s *RuntimeService) storeCache(ctx context.Context, model, digest string, turn value_objects.CachedTurn) {
	if s.cache == nil {
		return
	}
	s.cache.Put(ctx, model, digest, turn)
}

// digestRequest 返回提示词的字符数与整个请求的短指纹。
//
// 指纹取 sha256 的前 8 字节（16 个十六进制字符），有两个用途：
// 排查时判断两次发言看到的输入是不是同一份（指纹一样说明是模型在抖，
// 不一样说明素材变了），以及当作发言缓存的键。
// 这两个用途都不需要抗碰撞强度，16 个字符足够，也让轨迹文档小一点。
//
// # 为什么哈希的不只是提示词正文
//
// 因为缓存键必须覆盖**全部会改变产出的输入**。只哈希正文时有一个很难发现的故障：
// 发现某位成员的报告被 max_tokens 截断、调大 RuntimeConfig.MaxTokens 重新部署之后，
// 提示词一个字没变 → 指纹没变 → 24 小时内一直命中那份旧的截断结果，
// 改动看起来完全没生效。采样温度与工具授权同理。
//
// 消息之间写一个 0 字节分隔：否则 system="ab"+user="c" 与 system="a"+user="bc"
// 会得到同一个指纹。当前提示词结构固定，实际撞不上，但这一行的成本是零。
func digestRequest(req value_objects.ChatRequest) (int, string) {
	h := sha256.New()
	chars := 0
	for _, m := range req.Messages {
		chars += len([]rune(m.Content))
		h.Write([]byte(m.Content))
		h.Write([]byte{0})
	}
	// 采样参数与工具授权一并进键。工具只取名字：声明的描述与 schema
	// 是随代码走的，不会在同一个二进制里变化。
	fmt.Fprintf(h, "\x00t=%v\x00mt=%d\x00tr=%d", req.Temperature, req.MaxTokens, req.MaxToolRounds)
	for _, name := range req.Access.Names() {
		h.Write([]byte("\x00tool=" + name.String()))
	}
	return chars, hex.EncodeToString(h.Sum(nil)[:8])
}

// chat 是工具调用循环。
// 每一轮的输入都是上一轮的输出，它是一段对话而不是一次批处理，
// 不存在可以合并的批量形式。真正的风险是不终止，因此这里用 MaxToolRounds 硬性封顶，
// 并且最后一轮强制摘掉工具声明，逼模型给出文字结论——
// 没有这一步，一个陷入「我再查一次」循环的模型会一直转到 ctx 超时，
// 而那时所有已花费的 token 都拿不回任何结论。
//
// 同一轮里的多个工具调用则是真正的扇出，走 helpers/concurrency，不用裸 goroutine。
// client 与 model 由 Execute 解析好传进来，本方法不再自己 Resolve：
// 缓存键要用真实模型名，Execute 那边必须先拿到它，解析两次纯属重复。
func (s *RuntimeService) chat(
	ctx context.Context,
	client LLMClient,
	model string,
	req value_objects.ChatRequest,
	subject ToolInvocation,
) (resp value_objects.ChatResponse, err error) {
	// 解析出来的模型名统一在出口回填，而不是在下面七个 return 上各写一遍。
	// 这个循环的退出点会随着厂商的怪异行为继续增加（见下面那条「没给工具也硬发工具调用」），
	// 每加一个出口就要记得补一次 Model，漏掉的那一个不会报错，
	// 只会让某一类失败的轨迹里模型名神秘地空着。
	defer func() { resp.Model = model }()

	messages := append([]value_objects.Message(nil), req.Messages...)
	var usage value_objects.Usage

	maxRounds := req.MaxToolRounds
	for round := 0; round <= maxRounds; round++ {
		if err := ctx.Err(); err != nil {
			return value_objects.ChatResponse{Usage: usage}, custom_errors.Unavailable("模型调用已取消").Wrap(err)
		}

		// 最后一轮不再提供工具：模型没有工具可调，只能给出结论。
		final := round == maxRounds
		tools := req.Tools
		if final {
			tools = nil
		}

		res, err := client.Complete(ctx, value_objects.CompletionRequest{
			Model:       model,
			Messages:    messages,
			Tools:       tools,
			Temperature: req.Temperature,
			MaxTokens:   req.MaxTokens,
		})
		if err != nil {
			return value_objects.ChatResponse{Messages: messages, Usage: usage}, err
		}
		usage = usage.Plus(res.Usage)

		if !res.HasToolCalls() {
			return value_objects.ChatResponse{
				Content:    res.Content,
				Messages:   messages,
				Usage:      usage,
				ToolRounds: round,
				Truncated:  final && round > 0,
			}, nil
		}
		if final {
			// 少数厂商即使没收到工具声明也会硬发工具调用。到这一步只能就地收场，
			// 把已有文本当作结论并标记截断，让调用方知道这份报告可能不完整。
			s.log.Warn("工具调用轮数已达上限仍在请求工具",
				zap.Int("max_rounds", maxRounds), zap.Int("pending_calls", len(res.ToolCalls)))
			return value_objects.ChatResponse{
				Content:    res.Content,
				Messages:   messages,
				Usage:      usage,
				ToolRounds: round,
				Truncated:  true,
			}, nil
		}

		messages = append(messages, value_objects.AssistantToolCallMessage(res.Content, res.ToolCalls))
		results, err := s.invokeTools(ctx, res.ToolCalls, req.Access, subject)
		if err != nil {
			return value_objects.ChatResponse{Messages: messages, Usage: usage}, err
		}
		messages = append(messages, results...)
	}

	// maxRounds >= 0 时循环至少执行一次并在内部返回，走到这里只可能是逻辑写错了。
	return value_objects.ChatResponse{Messages: messages, Usage: usage},
		custom_errors.Internal("工具调用循环异常退出")
}

// invokeTools 执行同一轮里的全部工具调用，结果按原顺序返回。
//
// 用 Settle 而不是 Map：一个工具查不到数据是极其正常的事（这只票今天没新闻），
// 让它株连同一轮的其他工具毫无道理。失败的工具返回一条错误说明给模型，
// 模型看到「未取到数据」会自己调整策略——这比整位分析师直接失败好得多。
//
// 顺序必须与调用顺序一致：OpenAI 协议要求每个 tool 结果紧跟其 tool_call，
// 顺序错乱会被判为无效请求。Settle 保证结果按下标对齐，这里依赖的正是那条保证。
func (s *RuntimeService) invokeTools(
	ctx context.Context,
	calls []value_objects.ToolCall,
	access value_objects.DataAccess,
	subject ToolInvocation,
) ([]value_objects.Message, error) {
	outcomes, err := concurrency.Settle(ctx, calls, s.cfg.ToolFanOutLimit,
		func(ctx context.Context, call value_objects.ToolCall) (string, error) {
			return s.invokeOne(ctx, call, access, subject)
		})
	if err != nil {
		// Settle 只在父 ctx 被取消时报错，单个工具的失败在 outcomes 里。
		return nil, custom_errors.Unavailable("工具执行已取消").Wrap(err)
	}

	out := make([]value_objects.Message, 0, len(calls))
	for i, o := range outcomes {
		result := o.Value
		if o.Err != nil {
			// 工具失败以「工具结果」的形式回灌，而不是让整次发言失败：
			// 模型完全有能力在一个数据源缺失时换个角度论证。
			result = fmt.Sprintf("工具执行失败: %s。请不要猜测该数据，直接说明这一项缺失。",
				custom_errors.MessageOf(o.Err))
		}
		out = append(out, value_objects.ToolResultMessage(calls[i], s.clip(result)))
	}
	return out, nil
}

func (s *RuntimeService) invokeOne(
	ctx context.Context,
	call value_objects.ToolCall,
	access value_objects.DataAccess,
	subject ToolInvocation,
) (string, error) {
	// 授权在执行前再校验一次。只靠「没把声明发给模型」来限制工具是不够的：
	// 模型会幻觉出没见过的工具名，而提示词注入正是冲着越权取数来的。
	if !access.Allows(call.Name) {
		return "", custom_errors.Forbidden("工具 %s 未对该智能体授权", call.Name)
	}
	if s.tools == nil {
		return "", custom_errors.Internal("未配置工具注册表")
	}
	tool, ok := s.tools.Lookup(call.Name)
	if !ok {
		return "", custom_errors.NotFound("工具 %s 不存在", call.Name)
	}

	return tool.Invoke(ctx, ToolInvocation{
		Code:      subject.Code,
		TradeDate: subject.TradeDate,
		Arguments: call.Args(),
	})
}

// clip 截断过长的工具结果。
func (s *RuntimeService) clip(text string) string {
	rs := []rune(text)
	if len(rs) <= s.cfg.MaxToolResultRunes {
		return text
	}
	return string(rs[:s.cfg.MaxToolResultRunes]) + "\n…（结果过长已截断，如需更多请缩小查询范围）"
}

func (s *RuntimeService) specsFor(access value_objects.DataAccess) []value_objects.ToolSpec {
	if s.tools == nil || access.IsEmpty() {
		return nil
	}
	return s.tools.Specs(access)
}

func pickFloat(preferred, fallback float64) float64 {
	if preferred > 0 {
		return preferred
	}
	return fallback
}

func pickInt(preferred, fallback int) int {
	if preferred > 0 {
		return preferred
	}
	return fallback
}

// pickToolRounds 解析工具轮数。
//
// 契约里的 0 有两种含义，必须靠授权集合来区分：不带工具的成员（研究经理、风控经理）
// 的 0 是「确实不需要工具」，而带工具的成员的 0 是「没填，用默认值」。
// 混为一谈的后果是市场分析师一次工具都调不了，却又不报错——
// 它会安安静静地只用提示词里那点数据写完报告。
func pickToolRounds(contract value_objects.Contract, fallback int) int {
	if contract.Access.IsEmpty() {
		return 0
	}
	if contract.MaxToolRounds > 0 {
		return contract.MaxToolRounds
	}
	return fallback
}
