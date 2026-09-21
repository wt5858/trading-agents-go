package repositories

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

const (
	keyUserSlots   = "ta:slots:user:"
	keyGlobalSlots = "ta:slots:global"
	// keySlotTicket 是「某个任务持有一个名额」的凭据键。
	// 它把名额从一个匿名计数变成了可寻址的持有关系，见 Release 的说明。
	keySlotTicket = "ta:slots:ticket:"
)

// ConcurrencyGuard 是并发配额：限制单用户与全局同时进行的分析数。
//
// 它属于 repositories/：名额计数器是一份跨进程共享的持久化状态，
// 放在 domain_services/ 会退化成单实例内存计数，多副本部署下直接失效。
//
// 计数器带 TTL 而不是永久存在：worker 进程被 kill -9 时 Release 不会执行，
// 泄漏的名额会永久占着额度。TTL 让计数最终自愈——代价是极端情况下
// 长任务的名额会被提前释放，这是可接受的（MySQL 侧 CountRunningByUser 是兜底校验）。
type ConcurrencyGuard struct {
	rdb *redis.Client
	ttl time.Duration
}

func NewConcurrencyGuard(rdb *redis.Client, ttl time.Duration) *ConcurrencyGuard {
	if ttl <= 0 {
		// 默认 6 小时：远大于单次分析的最长耗时，只在进程非正常退出时兜底。
		ttl = 6 * time.Hour
	}
	return &ConcurrencyGuard{rdb: rdb, ttl: ttl}
}

func (g *ConcurrencyGuard) userKey(userID uint64) string {
	return keyUserSlots + strconv.FormatUint(userID, 10)
}

func (g *ConcurrencyGuard) ticketKey(taskID string) string { return keySlotTicket + taskID }

// acquireScript 原子地检查额度、发放凭据并自增计数。
//
// # 为什么必须是一个 Lua 脚本
//
// 「先 GET 看看还剩多少，再 INCRBY 占上」是典型的 TOCTOU：两个并发提交
// 会同时读到「还差一个名额」，然后双双通过，额度形同虚设。检查与自增之间
// 只要存在任何一个网络往返，这个窗口就存在。Lua 在 Redis 里单线程执行到底，
// 期间不会被插入其它命令，检查与占用因此是同一个不可分割的操作。
//
// 两个维度一起判定也是同理：先判用户额度再单独发一次请求判全局，会在全局超额时
// 留下已经自增过的用户计数，需要补偿回滚——而补偿本身又会失败。
//
// 凭据（ticket）也在同一个脚本里发放，保证「计数 +1」与「存在一张凭据」
// 这两件事同时成立，否则 Release 的去重依据就可能缺失。
//
// KEYS: [1]=用户计数 [2]=全局计数 [3..]=每个任务的凭据键（凭据数即申请的名额数）
// ARGV: [1]=用户上限 [2]=全局上限 [3]=TTL 秒
// 返回：0 成功；-1 用户额度不足；-2 全局额度不足。
var acquireScript = redis.NewScript(`
local userKey     = KEYS[1]
local globalKey   = KEYS[2]
local n           = #KEYS - 2
local userLimit   = tonumber(ARGV[1])
local globalLimit = tonumber(ARGV[2])
local ttl         = tonumber(ARGV[3])

if n <= 0 then return 0 end

local used = tonumber(redis.call('GET', userKey) or '0')
if userLimit > 0 and used + n > userLimit then
  return -1
end

local total = tonumber(redis.call('GET', globalKey) or '0')
if globalLimit > 0 and total + n > globalLimit then
  return -2
end

for i = 3, #KEYS do
  redis.call('SET', KEYS[i], '1', 'EX', ttl)
end
redis.call('INCRBY', userKey, n)
redis.call('EXPIRE', userKey, ttl)
redis.call('INCRBY', globalKey, n)
redis.call('EXPIRE', globalKey, ttl)
return 0
`)

// releaseScript 原子地归还名额，并保证每个名额只被归还一次。
//
// # 为什么靠凭据而不是直接 DECRBY
//
// 归还路径天然会被重复触发：可见性超时会让同一个任务被两个 worker 执行完，
// 用户主动取消和 worker 收尾也可能同时发生。直接 DECRBY 意味着一个名额被扣两次，
// 计数逐渐偏低，额度形同虚设（在 0 处封底只能防负数，防不住多扣）。
//
// DEL 返回「真正删掉了几个键」。凭据在 Acquire 时发放、在这里被删除，
// 因此只有第一次调用能拿到非零的删除数，后续重放拿到 0 并直接返回。
// 递减量等于删除数，而不是调用方声称的数量——「恰好一次」由 Redis 保证，
// 不依赖调用方的纪律。
//
// KEYS: [1]=用户计数 [2]=全局计数 [3..]=要归还的凭据键
// 返回：真正归还的名额数。
var releaseScript = redis.NewScript(`
local userKey   = KEYS[1]
local globalKey = KEYS[2]
if #KEYS <= 2 then return 0 end

local tickets = {}
for i = 3, #KEYS do
  tickets[#tickets + 1] = KEYS[i]
end

local freed = redis.call('DEL', unpack(tickets))
if freed <= 0 then return 0 end

-- 在 0 处封底：TTL 过期会让计数先行归零，此时凭据可能还在，
-- 减法必须不能把计数压成负数，否则后续的额度判定全废。
local used = tonumber(redis.call('GET', userKey) or '0')
if used - freed <= 0 then
  redis.call('DEL', userKey)
else
  redis.call('DECRBY', userKey, freed)
end

local total = tonumber(redis.call('GET', globalKey) or '0')
if total - freed <= 0 then
  redis.call('DEL', globalKey)
else
  redis.call('DECRBY', globalKey, freed)
end
return freed
`)

// Acquire 一次性为一组任务获取执行名额，超额时返回 CodeQuotaExceeded。
//
// 接收 taskIDs 而不是一个数量：批量提交需要「要么全拿到，要么一个都不拿」，
// 循环调用既是 N 次 RPC，又会在中途失败时留下需要回滚的半成品。
// 同时，名额与任务 ID 绑定正是 Release 能做到恰好一次的前提。
// userLimit / globalLimit <= 0 表示该维度不限流。
func (g *ConcurrencyGuard) Acquire(ctx context.Context, userID uint64, taskIDs []string, userLimit, globalLimit int) error {
	if len(taskIDs) == 0 {
		return nil
	}
	keys := make([]string, 0, len(taskIDs)+2)
	keys = append(keys, g.userKey(userID), keyGlobalSlots)
	for _, id := range taskIDs {
		keys = append(keys, g.ticketKey(id))
	}

	code, err := acquireScript.Run(ctx, g.rdb, keys,
		userLimit, globalLimit, int(g.ttl.Seconds()),
	).Int()
	if err != nil {
		return custom_errors.Internal("获取分析并发名额失败").Wrap(err)
	}
	switch code {
	case -1:
		return custom_errors.QuotaExceeded("并发分析数已达上限（%d），请等待进行中的任务完成", userLimit)
	case -2:
		// 全局超额不暴露具体数字：那是系统容量，对用户没有意义，也不该外泄。
		return custom_errors.QuotaExceeded("系统分析负载已满，请稍后再试")
	}
	return nil
}

// Release 归还一组任务占用的名额，返回真正归还的个数。
//
// 重复调用是安全的：第二次及以后一律返回 0 且不改动计数。调用方因此不需要
// 在各个收尾分支之间小心翼翼地协调「到底谁负责释放」——谁先到谁释放，其余是 no-op。
func (g *ConcurrencyGuard) Release(ctx context.Context, userID uint64, taskIDs ...string) (int, error) {
	if len(taskIDs) == 0 {
		return 0, nil
	}
	keys := make([]string, 0, len(taskIDs)+2)
	keys = append(keys, g.userKey(userID), keyGlobalSlots)
	for _, id := range taskIDs {
		keys = append(keys, g.ticketKey(id))
	}
	freed, err := releaseScript.Run(ctx, g.rdb, keys).Int()
	if err != nil {
		return 0, custom_errors.Internal("归还分析并发名额失败").Wrap(err)
	}
	return freed, nil
}

// InUse 返回该用户与全局当前占用的名额，供限流提示与运维观测使用。
func (g *ConcurrencyGuard) InUse(ctx context.Context, userID uint64) (user int64, global int64, err error) {
	pipe := g.rdb.Pipeline()
	userCmd := pipe.Get(ctx, g.userKey(userID))
	globalCmd := pipe.Get(ctx, keyGlobalSlots)
	// redis.Nil 表示计数器不存在，也就是 0，不是错误。
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return 0, 0, custom_errors.Internal("查询分析并发名额失败").Wrap(err)
	}
	return parseCount(userCmd), parseCount(globalCmd), nil
}

func parseCount(cmd *redis.StringCmd) int64 {
	n, err := cmd.Int64()
	if err != nil {
		return 0
	}
	return n
}
