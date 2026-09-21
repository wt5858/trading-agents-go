package mq

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// ConnectionInfo 是连接一个 AMQP broker 所需的全部信息。
//
// 拆成字段而不是直接收一个 URL：密码需要单独处理（日志里必须打码），
// 而从一个拼好的 URL 里把密码摘出来远比拼进去麻烦。
type ConnectionInfo struct {
	Host     string
	Port     int
	VHost    string
	User     string
	Password string
}

func (c ConnectionInfo) normalized() ConnectionInfo {
	if c.Host == "" {
		c.Host = "127.0.0.1"
	}
	if c.Port == 0 {
		c.Port = 5672
	}
	if c.VHost == "" {
		c.VHost = "/"
	}
	if c.User == "" {
		c.User = "guest"
		c.Password = "guest"
	}
	return c
}

// URL 拼出 amqp:// 连接串。
//
// vhost 必须整体转义：默认 vhost 是 "/"，不转义的话路径会变成空串，
// broker 会当成「连接到名为空的 vhost」而不是默认 vhost，报错信息还相当难懂。
func (c ConnectionInfo) URL() string {
	c = c.normalized()
	return fmt.Sprintf("amqp://%s:%s@%s:%d/%s",
		url.QueryEscape(c.User), url.QueryEscape(c.Password),
		c.Host, c.Port, url.PathEscape(strings.TrimPrefix(c.VHost, "/")))
}

// SafeURL 是可以写进日志的版本：密码被打码。
func (c ConnectionInfo) SafeURL() string {
	c = c.normalized()
	return fmt.Sprintf("amqp://%s:***@%s:%d/%s",
		c.User, c.Host, c.Port, strings.TrimPrefix(c.VHost, "/"))
}

// Exchange 是一个交换机声明。
type Exchange struct {
	Name    string
	Type    string // direct / topic / fanout
	Durable bool
}

func (e Exchange) kind() string {
	if e.Type == "" {
		return "direct"
	}
	return e.Type
}

// Queue 是一个队列声明，连同它的绑定与重试策略。
//
// # 为什么重试参数长在队列上而不是客户端上
//
// 「失败之后等多久重来、重几次」是每条业务链路各不相同的判断：
// 一次定时任务的重投可以等 30 秒（反正下一次触发在几分钟后），
// 而一条要求低延迟的请求消息等 30 秒就已经没有意义了。
// 把它放在客户端上，等于强迫所有链路共用一个必然对某些链路是错的数字。
type Queue struct {
	// Exchange / RoutingKey 是这个队列的绑定。二者留空表示只用默认交换机
	// （按队列名直投），重试队列与死信队列就是这么工作的。
	Exchange   string
	Name       string
	RoutingKey string
	Durable    bool

	// Prefetch 是单个消费者未确认消息的上限。
	//
	// 默认 1：本服务的消息粒度很粗（一条消息可能触发几分钟的 LLM 调用或行情同步），
	// 预取多条只会让它们在一个消费者本地排队，而旁边的副本闲着。
	// 预取值真正该调大的场景是「每条消息处理耗时以毫秒计」，本服务没有这种队列。
	Prefetch int

	// Consumers 是本进程为这个队列起的并发消费者数量。
	Consumers int

	// MaxRetries 是处理失败后最多重投几次，超过则进死信队列。
	MaxRetries int

	// RetryDelay 是每次重投之前的等待时长，由重试队列的 TTL 实现。
	RetryDelay time.Duration
}

func (q Queue) normalized(def Options) Queue {
	if q.Prefetch <= 0 {
		q.Prefetch = 1
	}
	if q.Consumers <= 0 {
		q.Consumers = 1
	}
	if q.MaxRetries <= 0 {
		q.MaxRetries = def.MaxRetries
	}
	if q.RetryDelay <= 0 {
		q.RetryDelay = def.RetryDelay
	}
	return q
}

// RetryName / DLQName 是重试队列与死信队列的名字。
//
// 由主队列名派生而不是让调用方各起一个：名字一旦可以自由起，
// 就一定会出现 queue.foo 配了 queue.foo_retry、queue.bar 配了 queue.bar.retries
// 这种情况，而排查问题的人要先猜对名字才能看到积压。
func (q Queue) RetryName() string { return q.Name + ".retry" }
func (q Queue) DLQName() string   { return q.Name + ".dlq" }
