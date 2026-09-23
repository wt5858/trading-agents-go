package repositories

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
)

// completionCachePrefix 是缓存键前缀。
const completionCachePrefix = "agent:turn:"

// defaultCompletionTTL 是缓存条目的保鲜期。
//
// 24 小时不是「结论 24 小时内有效」的意思——那个问题由键本身回答（见下）。
// 它纯粹是内存上限：一次分析十四条，批量分析一天能写进去几千条，
// 没有 TTL 的话 Redis 会随历史任务无限增长。
const defaultCompletionTTL = 24 * time.Hour

// CompletionCache 缓存一位成员一次完整发言的产出。
//
// ===========================================================================
// 为什么键是提示词指纹，而不是「股票 + 日期 + 成员」
// ===========================================================================
//
// 后者是错的，而且错得很隐蔽：同一只票同一天同一位分析师，盘中跑和收盘后跑
// 看到的行情不一样，结论理应不同；用「股票+日期+成员」当键会让下午三点的
// 那次分析原样返回上午十点的结论，而且看不出任何异常。
//
// 提示词指纹（sha256 前 8 字节，在 RuntimeService 渲染完就算好）是**全部输入**
// 的摘要——行情、指标、上游报告，任何一项变了指纹就变了。于是「什么时候该失效」
// 这个问题根本不需要回答：输入没变才命中，而输入没变时结论本来就该一样。
//
// ===========================================================================
// 缓存的不是消耗
// ===========================================================================
//
// 命中时 Usage 必须归零，绝不能把原来那次的 token 数一起返回。
// 原样返回的话，一次全命中的分析会在 analysis_tasks.result.usage 里
// 报出几万 token 和几毛钱成本——而这次运行一个字都没发给模型。
// 成本统计就此系统性虚高，且越省钱的部署虚得越厉害。
// 命中这件事本身记在轨迹的 CacheHit 上，要看省了多少去那里数。
type CompletionCache struct {
	rdb *redis.Client
	ttl time.Duration
}

func NewCompletionCache(rdb *redis.Client, ttl time.Duration) *CompletionCache {
	if ttl <= 0 {
		ttl = defaultCompletionTTL
	}
	return &CompletionCache{rdb: rdb, ttl: ttl}
}

// cachedTurn 是落进 Redis 的形态。
//
// 只存产出，不存消耗：消耗属于「这次运行花了多少」，而缓存命中意味着没花。
type cachedTurn struct {
	Content    string `json:"content"`
	ToolRounds int    `json:"tool_rounds"`
	Truncated  bool   `json:"truncated"`
	Model      string `json:"model"`
}

func (c *CompletionCache) key(model, promptDigest string) string {
	return completionCachePrefix + model + ":" + promptDigest
}

// Get 取一条缓存。第二个返回值表示是否命中。
//
// 任何异常（Redis 挂了、反序列化失败）都按「未命中」处理，不返回 error：
// 缓存是纯粹的加速手段，让一次分析因为缓存读不到而失败毫无道理。
func (c *CompletionCache) Get(ctx context.Context, model, promptDigest string) (value_objects.CachedTurn, bool) {
	if c.rdb == nil || model == "" || promptDigest == "" {
		return value_objects.CachedTurn{}, false
	}
	raw, err := c.rdb.Get(ctx, c.key(model, promptDigest)).Bytes()
	if err != nil {
		// redis.Nil 是正常的未命中，其余是故障——两者在这里的处理相同。
		return value_objects.CachedTurn{}, false
	}
	var row cachedTurn
	if err := json.Unmarshal(raw, &row); err != nil {
		return value_objects.CachedTurn{}, false
	}
	if row.Content == "" {
		// 空产出不该被当作有效缓存：它一旦被写进去，会让这位成员
		// 在 TTL 内每次都「成功地」返回空报告。
		return value_objects.CachedTurn{}, false
	}
	return value_objects.CachedTurn{
		Content:    row.Content,
		ToolRounds: row.ToolRounds,
		Truncated:  row.Truncated,
		Model:      row.Model,
	}, true
}

// Put 写入一条缓存。写失败只当没缓存，不影响本次分析。
//
// 只缓存有正文的产出：空内容在 Act 那边就算失败，缓存它等于
// 把一次偶发的空返回固化成 TTL 内的必然失败。
func (c *CompletionCache) Put(ctx context.Context, model, promptDigest string, turn value_objects.CachedTurn) {
	if c.rdb == nil || model == "" || promptDigest == "" || turn.Content == "" {
		return
	}
	// 撞到工具轮数上限的产出是不完整的，不进缓存：
	// 它被复用 24 小时的代价，远高于重跑一次的那点 token。
	if turn.Truncated {
		return
	}
	raw, err := json.Marshal(cachedTurn{
		Content:    turn.Content,
		ToolRounds: turn.ToolRounds,
		Truncated:  turn.Truncated,
		Model:      turn.Model,
	})
	if err != nil {
		return
	}
	_ = c.rdb.Set(ctx, c.key(model, promptDigest), raw, c.ttl).Err()
}
