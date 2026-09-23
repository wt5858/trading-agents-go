package httpx

import (
	"errors"
	"io"
	"math/rand"
	"strings"
	"time"
)

// IsRetriableOutbound 判断一个传输层错误值不值得重试。
//
// 只认「连接被对端掐断」这一类：限流的常见表现就是连接直接关掉，Go 侧拿到的是
// io.EOF / io.ErrUnexpectedEOF，或者 syscall 的 ECONNRESET 被包成字符串。
// 这些重试有意义。
//
// 刻意不重试 DNS 解析失败、证书错误、连接被拒：它们重试一百次也是一样的结果，
// 只会把一次快速失败拖成几十秒。同样不重试超时——对端慢，再发一遍通常还是慢，
// 而每一次都要等满整个超时，代价是所有重试里最贵的。ctx 取消更不能重试，
// 那是调用方明确要求停下来（调用方应当在进到这里之前先查 ctx.Err()）。
//
// # 为什么这个判据住在 httpx 而不是某个调用方里
//
// 它原本只在行情出站层有一份。LLM 那边各写各的，结果是「所有传输错误一律重试」：
// 单次超时 180 秒、重试 3 次，一个打不通的域名能让一次模型调用耗掉 12 分钟，
// 而一次分析里有十几次这样的调用。两处出站、两套重试判据，分叉只是时间问题——
// 所以判据只留一份，谁出站谁用它。
func IsRetriableOutbound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	// net.OpError 里的 ECONNRESET 在各平台上的具体类型不统一，退一步按文本判断。
	// 匹配的是 Go 标准库自己产生的固定措辞，不是上游报文，所以不怕对方改文案。
	msg := strings.ToLower(err.Error())
	for _, s := range []string{"connection reset by peer", "unexpected eof", "server closed idle connection"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// Jitter 给退避时长加上 [0, d) 的随机抖动。
//
// 没有抖动的指数退避在并发场景下等于把所有失败请求排成一队重放：几十个并发请求
// 同一瞬间吃到同一个 429，然后在 t+500ms 整齐地一起醒来，再一起被限流一次。
// 抖动把这个同步的尖峰摊开，代价只是每次多等一点。
//
// 用 math/rand 而不是 crypto/rand 是刻意的：这里要的是「别撞在一起」，不是不可预测。
func Jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d + time.Duration(rand.Int63n(int64(d)))
}
