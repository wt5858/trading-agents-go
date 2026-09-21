package value_objects

import (
	"encoding/json"
	"math"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// maxPayloadBytes 是参数包的落库上限。
//
// 参数包是「运行器需要的开关」，不是数据通道。没有上限的话，
// 迟早会有人把一份股票代码全集塞进来，让 scheduled_jobs 变成一张大字段表，
// 而这张表是调度器每个 tick 都要扫的——行宽直接决定扫描成本。
const maxPayloadBytes = 16 * 1024

// JobPayload 是调度器不解释、只搬运的任务参数。
//
// # 为什么是不透明的 map
//
// 调度上下文**必须**不知道 market_sync 需要哪些参数、scheduled_analysis 需要哪些参数。
// 一旦它开始理解参数结构，就得在编译期依赖 stock / analysis 的类型，
// 限界上下文之间的隔离当场破功。参数的语义属于运行器，校验也属于运行器。
//
// # 为什么仍然是值对象而不是裸 map
//
// 裸 map 是共享可变状态：仓储读出来的 map 交给运行器，运行器一改，
// 聚合里的参数就跟着变了，而且没有任何写入路径被触发。
// 这里的构造器与访问器全部走拷贝，把「不可变」这件事变成结构上的保证。
type JobPayload struct {
	v map[string]any
}

// NewJobPayload 构造参数包，nil 与空 map 都是合法的（大量任务确实不需要参数）。
//
// 这里做浅拷贝：顶层键值不再与调用方共享。嵌套的 map/slice 仍然是共享的，
// 但参数包的约定用法是「读一次、传给运行器」，为此做一次深拷贝的开销不划算——
// 真正会咬人的是顶层被整体替换，浅拷贝已经挡住了。
func NewJobPayload(m map[string]any) (JobPayload, error) {
	if len(m) == 0 {
		return JobPayload{}, nil
	}
	copied := make(map[string]any, len(m))
	for k, v := range m {
		copied[k] = v
	}
	p := JobPayload{v: copied}
	raw, err := json.Marshal(copied)
	if err != nil {
		// 参数来自 HTTP 的 JSON 反序列化，正常情况下必然可再序列化。
		// 走到这里说明调用方塞了 chan/func 之类的东西，属于编程错误。
		return JobPayload{}, custom_errors.Invalid("任务参数无法序列化: %v", err)
	}
	if len(raw) > maxPayloadBytes {
		return JobPayload{}, custom_errors.Invalid("任务参数过大：%d 字节，上限 %d 字节", len(raw), maxPayloadBytes)
	}
	return p, nil
}

// ParseJobPayload 从 JSON 文本构造，供写路径（HTTP 传了原始 JSON 串）使用。
func ParseJobPayload(raw []byte) (JobPayload, error) {
	if len(raw) == 0 {
		return JobPayload{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return JobPayload{}, custom_errors.Invalid("任务参数不是合法的 JSON 对象: %v", err)
	}
	return NewJobPayload(m)
}

// RehydrateJobPayload 从库里的列重建，**不报错**。理由同 RehydrateCronExpression：
// 库里的行是既成事实，一条坏掉的 JSON 列不该让整个任务列表接口挂掉。
func RehydrateJobPayload(raw []byte) JobPayload {
	if len(raw) == 0 {
		return JobPayload{}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return JobPayload{}
	}
	return JobPayload{v: m}
}

// ToJSON 返回落库形态。空参数包返回 nil，让列保持 NULL 而不是写一串 "null"。
func (p JobPayload) ToJSON() ([]byte, error) {
	if len(p.v) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(p.v)
	if err != nil {
		return nil, custom_errors.Internal("任务参数序列化失败").Wrap(err)
	}
	return raw, nil
}

// Map 返回一份拷贝。返回拷贝而不是内部 map，是「不可变」的最后一道闸门。
func (p JobPayload) Map() map[string]any {
	out := make(map[string]any, len(p.v))
	for k, v := range p.v {
		out[k] = v
	}
	return out
}

// Lookup 取原始值。运行器自己负责断言类型——参数的语义归运行器。
func (p JobPayload) Lookup(key string) (any, bool) {
	v, ok := p.v[key]
	return v, ok
}

// Str 是运行器最常用的取值方式，省掉每个运行器各写一遍类型断言。
func (p JobPayload) Str(key string) string {
	s, _ := p.v[key].(string)
	return s
}

// Int 统一处理 JSON 数字：encoding/json 把所有数字解成 float64，
// 而运行器想要的总是整数（userId、depth、limit）。这个转换写一次好过每个运行器写一次。
//
// 返回 int64 而不是 float64：payload 里的数值参数没有一个是金额或比率，
// 全都是标识与计数。让它返回浮点，只会逼着每个调用点再写一次 int(x) 转换，
// 而那次转换是静默截断——一个写错成 1e20 的 userId 会被截成一个看起来正常的数。
func (p JobPayload) Int(key string) (int64, bool) {
	switch n := p.v[key].(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		// 走到这里说明解码时没开 UseNumber。超出 int64 精确表示范围的值
		// 转换后已经不是原值，宁可判为「取不到」也不要给出一个错的标识。
		if n != math.Trunc(n) || math.Abs(n) > math.MaxInt64 {
			return 0, false
		}
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}

func (p JobPayload) Len() int { return len(p.v) }

func (p JobPayload) IsZero() bool { return len(p.v) == 0 }
