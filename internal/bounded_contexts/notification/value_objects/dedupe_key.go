package value_objects

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

const (
	// MaxDedupeKeyLen 与 notifications.dedupe_key 的列宽（varchar(160)）一致。
	//
	// 上限是必须的：这一列进了 (user_id, dedupe_key) 唯一索引，而 InnoDB 的索引键
	// 有长度上限。列宽定死之后，超长的来源标识必须在**构造点**就被收敛掉——
	// 留到 INSERT 时才被数据库截断，两个不同的来源就会截出同一个键，
	// 于是第二条本该独立的通知被当成重放静默丢弃，而且没有任何痕迹。
	MaxDedupeKeyLen = 160

	// dedupeSeparator 分隔种类与来源标识。
	dedupeSeparator = ":"

	// dedupeHashPrefix 标记「来源标识过长，已被哈希」的键。
	// 留一个可见前缀是为了让运维在库里一眼分辨出这是哈希值而不是某个真实 ID。
	dedupeHashPrefix = "#"
)

// DedupeKey 是通知的去重键：由「通知种类 + 来源聚合标识」推导出的稳定字符串。
//
// ===========================================================================
// 这个值对象是整个上下文的地基
// ===========================================================================
//
// 领域事件是**至少一次**投递的：总线重启后的补投、worker 可见性超时导致的重复执行、
// 将来换成 MQ 之后的重投，都会让同一个 OnTaskCompleted 到达通知处理器不止一次。
// 没有去重键的话，用户会为同一个分析任务收到三条一模一样的「分析完成」。
//
// 幂等的实现分三层，这个 VO 是第一层：
//
//  1. DedupeKey（这里）——把「哪件事」压成一个确定的字符串；
//  2. (user_id, dedupe_key) 唯一索引——在并发下真正挡住第二次写入；
//  3. 事件处理器把 AlreadyExists 当成功——让重放安静地变成无操作。
//
// # 「稳定」具体指什么
//
// 同一个来源事件，无论重放多少次、在哪台机器上、隔多久，算出的键必须逐字节相同。
// 因此这里**只**取 (kind, sourceID) 两个输入：
//
//   - 不取时间：重放发生在另一个时刻，带上时间的键每次都不同，去重直接失效。
//   - 不取事件 ID：BaseDomainEvent.ID 是每次 New 出来的 uuid。同一次业务事实被
//     重新构造并重投时（比如补偿任务重扫一遍已完成的任务），事件 ID 会是新的，
//     而它描述的仍是同一件事。用事件 ID 去重，只能挡住「同一个内存对象被投了两次」，
//     挡不住真正会发生的那种重复。
//   - 不取标题正文：文案是会改的。改一次文案就让全部历史通知重新推一遍，荒唐。
//
// # 为什么 kind 也要进键
//
// 只用 sourceID 的话，同一个分析任务的「失败」通知会挡住它重试成功后的「完成」通知。
// 两者是两件不同的事，理应各有一条。
type DedupeKey struct{ v string }

// NewDedupeKey 由通知种类与来源聚合标识推导去重键。
//
// sourceID 是**来源聚合的标识**（分析任务 ID、同步批次 ID、定时任务 ID……），
// 不是通知自己的 ID——通知此刻还不存在。系统公告这类没有来源聚合的通知，
// 由调用方给出一个自己保证稳定的标识（公告编号），而不是随手塞一个 uuid：
// 塞 uuid 等于宣布「这条通知永远不会重复」，那正是去重键存在的意义被绕开的地方。
func NewDedupeKey(kind NotificationKind, sourceID string) (DedupeKey, error) {
	if !kind.Valid() {
		return DedupeKey{}, custom_errors.Invalid("推导去重键失败：非法的通知种类 %q", kind.String())
	}
	// 只做首尾去空白，不做大小写归一：来源标识是别的上下文的主键，
	// 把 "600519.SH" 与 "600519.sh" 归成一个是我们无权做的假设，
	// 万一两者确实是两个不同的聚合，归一之后第二条通知会被静默吞掉。
	id := strings.TrimSpace(sourceID)
	if id == "" {
		return DedupeKey{}, custom_errors.Invalid("推导去重键失败：来源标识不能为空")
	}

	key := kind.String() + dedupeSeparator + id
	if len(key) <= MaxDedupeKeyLen {
		return DedupeKey{v: key}, nil
	}

	// 超长时退化成哈希，而不是截断也不是报错：
	//   - 截断会让两个前缀相同的长 ID 撞成同一个键（见 MaxDedupeKeyLen 的注释）；
	//   - 报错会让一条本该送达的通知因为来源 ID 太长而彻底消失，而通知的失败
	//     不该有这种「让用户少收到东西」的后果。
	// sha256 是纯函数，同样的输入永远得到同样的输出，稳定性不受影响。
	sum := sha256.Sum256([]byte(key))
	return DedupeKey{v: kind.String() + dedupeSeparator + dedupeHashPrefix + hex.EncodeToString(sum[:])}, nil
}

// RehydrateDedupeKey 跳过校验，仅供从数据库重建使用。理由见 RehydrateNotificationKind。
func RehydrateDedupeKey(s string) DedupeKey { return DedupeKey{v: s} }

func (k DedupeKey) String() string { return k.v }

func (k DedupeKey) IsZero() bool { return k.v == "" }
