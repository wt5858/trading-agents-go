package repositories

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

const (
	progressChannelPrefix = "ta:progress:"
	progressSnapshotKey   = "ta:progress:snapshot:"
)

// ProgressPublisher 用 Redis Pub/Sub + 快照键实现进度推送，供 SSE 订阅。
//
// 它属于 repositories/ 而非 domain_services/：进度快照是一份带 TTL 的持久化状态，
// 谁写它、写到哪、怎么过期，全是存储决策。
//
// 为什么 Pub/Sub 之外还要写一个快照键：Pub/Sub 是「发了就没」的语义，
// 一个在任务跑到 60% 时才打开页面的 SSE 订阅者，如果只订阅频道，
// 要等到下一次步骤推进（可能是几十秒后）才能看到任何东西，页面会一直空白。
// 有了快照，订阅者可以先 GET 一次拿到当前状态，再接上增量。
//
// 快照带 TTL 而不是永久保存：任务完成后快照就没用了（终态在 MySQL 里），
// 留着只会让 Redis 内存随历史任务无限增长。
type ProgressPublisher struct {
	rdb *redis.Client
	ttl time.Duration
}

func NewProgressPublisher(rdb *redis.Client, ttl time.Duration) *ProgressPublisher {
	if ttl <= 0 {
		// 默认 2 小时：远长于一次分析的耗时，又短到不会让快照堆积。
		ttl = 2 * time.Hour
	}
	return &ProgressPublisher{rdb: rdb, ttl: ttl}
}

func (p *ProgressPublisher) channel(taskID string) string {
	return progressChannelPrefix + taskID
}

func (p *ProgressPublisher) snapshotKey(taskID string) string {
	return progressSnapshotKey + taskID
}

// Publish 在一次往返里同时刷新快照与广播。
// 用 pipeline 而不是两次调用：进度推进在长任务里是高频动作，省一半 RPC。
//
// 推送出去的是聚合里那一份已经固化好派生量的 Progress，
// 与落库的字节完全一致——前端刷新页面前后看到的百分比因此不会跳。
func (p *ProgressPublisher) Publish(ctx context.Context, taskID string, prog value_objects.Progress) error {
	if taskID == "" {
		return custom_errors.Invalid("任务 ID 不能为空")
	}
	payload, err := json.Marshal(prog)
	if err != nil {
		return custom_errors.Internal("进度序列化失败").Wrap(err)
	}
	pipe := p.rdb.TxPipeline()
	// 先写快照再广播：反过来的话，订阅者收到消息后立刻去读快照，可能读到上一帧。
	pipe.Set(ctx, p.snapshotKey(taskID), payload, p.ttl)
	pipe.Publish(ctx, p.channel(taskID), payload)
	if _, err := pipe.Exec(ctx); err != nil {
		return custom_errors.Internal("推送任务进度失败").Wrap(err)
	}
	return nil
}

// Snapshot 读当前进度快照。不存在返回 (nil, nil)：
// 「还没有任何进度」是正常状态（任务刚排队），不是错误。
//
// 返回的是落库时算好的 Percent / ETASeconds，这里不做任何重算。
func (p *ProgressPublisher) Snapshot(ctx context.Context, taskID string) (*value_objects.Progress, error) {
	raw, err := p.rdb.Get(ctx, p.snapshotKey(taskID)).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, custom_errors.Internal("读取任务进度快照失败").Wrap(err)
	}
	var prog value_objects.Progress
	if err := json.Unmarshal(raw, &prog); err != nil {
		return nil, custom_errors.Internal("解析任务进度快照失败").Wrap(err)
	}
	return &prog, nil
}

// Subscribe 订阅某个任务的后续进度。
//
// 刻意不在这里先塞一帧快照：调用方该先 Snapshot 再 Subscribe，
// 把「当前状态」和「后续增量」两件事分开，语义更清楚也更好测。
//
// 返回的 ProgressStream 必须 Close，否则 Redis 端的订阅连接会泄漏。
func (p *ProgressPublisher) Subscribe(ctx context.Context, taskID string) (*ProgressStream, error) {
	if taskID == "" {
		return nil, custom_errors.Invalid("任务 ID 不能为空")
	}
	sub := p.rdb.Subscribe(ctx, p.channel(taskID))
	// 等待订阅真正建立。不等的话，紧随其后发布的进度会落进黑洞。
	if _, err := sub.Receive(ctx); err != nil {
		_ = sub.Close()
		return nil, custom_errors.Internal("订阅任务进度失败").Wrap(err)
	}
	return &ProgressStream{sub: sub, ch: sub.Channel()}, nil
}

// ProgressStream 是一条进度订阅，用「拉」而不是「推」的形状暴露。
//
// # 为什么不是 <-chan Progress
//
// 返回通道就必须有一个常驻协程把 Redis 的消息泵进去、解码、再处理慢订阅者——
// 那是一个裸 goroutine，而且它的生命周期与调用方的 HTTP 连接耦合，
// 一旦调用方忘记取消就永久泄漏。
//
// 改成 Next(ctx) 之后，解码发生在调用方自己的协程上（SSE 处理器本来就阻塞在那里等），
// 整条链路一个额外协程都不需要，取消语义也直接由调用方的 ctx 决定。
// 顺带去掉了「缓冲满了丢帧」那套逻辑：慢订阅者现在只是慢，不会丢中间帧。
type ProgressStream struct {
	sub *redis.PubSub
	ch  <-chan *redis.Message
}

// Next 阻塞取下一帧进度。
// ok == false 表示订阅已结束（连接关闭或 ctx 取消），调用方应停止循环。
func (s *ProgressStream) Next(ctx context.Context) (value_objects.Progress, bool, error) {
	for {
		select {
		case <-ctx.Done():
			return value_objects.Progress{}, false, nil
		case msg, alive := <-s.ch:
			if !alive {
				return value_objects.Progress{}, false, nil
			}
			var prog value_objects.Progress
			if err := json.Unmarshal([]byte(msg.Payload), &prog); err != nil {
				// 单条坏消息不该终止整个订阅：跳过它，下一帧照样能用。
				continue
			}
			return prog, true, nil
		}
	}
}

func (s *ProgressStream) Close() error {
	if s == nil || s.sub == nil {
		return nil
	}
	return s.sub.Close()
}
