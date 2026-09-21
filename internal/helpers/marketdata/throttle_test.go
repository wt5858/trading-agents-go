package marketdata

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// fakeRoundTripper 按脚本依次返回结果，并记下被调用了几次。
type fakeRoundTripper struct {
	errs  []error // 第 i 次调用返回 errs[i]；nil 表示成功
	calls int
}

func (f *fakeRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	i := f.calls
	f.calls++
	if i < len(f.errs) && f.errs[i] != nil {
		return nil, f.errs[i]
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}"))}, nil
}

// newGetRequest 按生产里的方式构造请求。
//
// 刻意不用 httptest.NewRequest：它走的是「解析一段报文」的路子，即便没有 body
// 也会给 req.Body 填一个非 nil 的值，于是「这个请求能不能重发」的判断在测试里
// 和在生产里表现不一致——而那正是这组用例要盯的东西。
func newGetRequest(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "https://example.invalid/x", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	return req
}

// newTestThrottle 造一个不真睡觉、不随机的 transport，这样测试是确定且快的。
func newTestThrottle(base http.RoundTripper, attempts int) (*throttleTransport, *[]time.Duration) {
	var slept []time.Duration
	return &throttleTransport{
		base: base,
		// rate.Inf 让 Wait 立刻返回：这些用例测的是重试与取消，不是限速本身。
		limiter:     rate.NewLimiter(rate.Inf, 1),
		maxAttempts: attempts,
		backoff:     100 * time.Millisecond,
		sleep:       func(d time.Duration) { slept = append(slept, d) },
		jitter:      func() float64 { return 0 },
	}, &slept
}

// TestThrottleRetriesOnConnectionReset 东财限流的表现就是把连接掐掉，
// 客户端只看到一个 EOF。不重试的话，一次 60 页的全市场扫描几乎不可能跑完。
func TestThrottleRetriesOnConnectionReset(t *testing.T) {
	base := &fakeRoundTripper{errs: []error{io.EOF, io.ErrUnexpectedEOF, nil}}
	tr, slept := newTestThrottle(base, 3)

	resp, err := tr.RoundTrip(newGetRequest(t))
	if err != nil {
		t.Fatalf("第三次应当成功: %v", err)
	}
	_ = resp.Body.Close()
	if base.calls != 3 {
		t.Fatalf("调用了 %d 次，期望 3 次", base.calls)
	}
	// 退避必须是递增的，否则「退避」只是个名字。
	if len(*slept) != 2 || (*slept)[1] <= (*slept)[0] {
		t.Fatalf("退避时长应递增，实际 %v", *slept)
	}
}

// TestThrottleGivesUpAfterMaxAttempts 重试不能无限：连续失败说明是封禁不是抖动，
// 继续打只会把封禁延长。
func TestThrottleGivesUpAfterMaxAttempts(t *testing.T) {
	base := &fakeRoundTripper{errs: []error{io.EOF, io.EOF, io.EOF, io.EOF}}
	tr, _ := newTestThrottle(base, 3)

	if _, err := tr.RoundTrip(newGetRequest(t)); err == nil {
		t.Fatal("期望返回错误")
	}
	if base.calls != 3 {
		t.Fatalf("调用了 %d 次，期望止于 3 次", base.calls)
	}
}

// TestThrottleDoesNotRetryNonTransient DNS 错误重试一百次也是一样的结果，
// 只会把一次快速失败拖成几十秒。
func TestThrottleDoesNotRetryNonTransient(t *testing.T) {
	base := &fakeRoundTripper{errs: []error{errors.New("no such host")}}
	tr, _ := newTestThrottle(base, 3)

	if _, err := tr.RoundTrip(newGetRequest(t)); err == nil {
		t.Fatal("期望返回错误")
	}
	if base.calls != 1 {
		t.Fatalf("调用了 %d 次，不可重试的错误应当只试 1 次", base.calls)
	}
}

// opaqueReader 是标准库无法为之生成 GetBody 的请求体（不是 bytes/strings Reader）。
type opaqueReader struct{ r io.Reader }

func (o opaqueReader) Read(p []byte) (int, error) { return o.r.Read(p) }

// TestThrottleDoesNotReplayUnrewindableBody 倒不回去的请求体一旦重发就是个空体请求，
// 而那比失败更糟——它会成功。
func TestThrottleDoesNotReplayUnrewindableBody(t *testing.T) {
	base := &fakeRoundTripper{errs: []error{io.EOF, nil}}
	tr, _ := newTestThrottle(base, 3)

	req, err := http.NewRequest(http.MethodPost, "https://example.invalid/x",
		opaqueReader{r: strings.NewReader(`{"a":1}`)})
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if req.GetBody != nil {
		t.Fatal("这个用例需要一个拿不到 GetBody 的请求体")
	}
	if _, err := tr.RoundTrip(req); err == nil {
		t.Fatal("期望返回错误而不是重发一个空体请求")
	}
	if base.calls != 1 {
		t.Fatalf("调用了 %d 次，倒不回去的 body 不该重试", base.calls)
	}
}

// TestThrottleReplaysRewindableBody 标准库给 strings/bytes Reader 自动生成了 GetBody，
// 这类请求可以安全重发——重发时 body 必须是完整的，不能是被读干的空流。
func TestThrottleReplaysRewindableBody(t *testing.T) {
	const payload = `{"a":1}`
	var seen []string
	base := &recordingRoundTripper{errs: []error{io.EOF, nil}, bodies: &seen}
	tr, _ := newTestThrottle(base, 3)

	req, err := http.NewRequest(http.MethodPost, "https://example.invalid/x", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("可重放的请求应当重试成功: %v", err)
	}
	_ = resp.Body.Close()
	if len(seen) != 2 {
		t.Fatalf("应当发出 2 次，实际 %d 次", len(seen))
	}
	if seen[1] != payload {
		t.Fatalf("重发的请求体是 %q，期望完整的 %q", seen[1], payload)
	}
}

// recordingRoundTripper 在 fakeRoundTripper 之上把每次看到的请求体记下来。
type recordingRoundTripper struct {
	errs   []error
	calls  int
	bodies *[]string
}

func (f *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	i := f.calls
	f.calls++
	got := ""
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		got = string(b)
	}
	*f.bodies = append(*f.bodies, got)
	if i < len(f.errs) && f.errs[i] != nil {
		return nil, f.errs[i]
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}"))}, nil
}

// TestThrottleRespectsContextCancellation 限速排队时如果调用方已经取消，
// 必须立刻让出，否则 concurrency.Settle 的取消传播会被拖住。
func TestThrottleRespectsContextCancellation(t *testing.T) {
	base := &fakeRoundTripper{}
	tr, _ := newTestThrottle(base, 3)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := newGetRequest(t).WithContext(ctx) //nolint:staticcheck // 测试里就是要换 ctx

	if _, err := tr.RoundTrip(req); !errors.Is(err, context.Canceled) {
		t.Fatalf("期望 context.Canceled，得到 %v", err)
	}
	if base.calls != 0 {
		t.Fatalf("已取消的请求不该发出去，实际发了 %d 次", base.calls)
	}
}

// ---------------------------------------------------------------------------
// 服务端错误的重试
// ---------------------------------------------------------------------------

// statusRoundTripper 按脚本依次返回状态码，并记录响应体有没有被关掉。
type statusRoundTripper struct {
	codes  []int
	calls  int
	closed int
}

type countingBody struct {
	io.Reader
	onClose func()
}

func (b *countingBody) Close() error { b.onClose(); return nil }

func (f *statusRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	i := f.calls
	f.calls++
	code := 200
	if i < len(f.codes) {
		code = f.codes[i]
	}
	return &http.Response{
		StatusCode: code,
		// 502 页面带着一大段 padding，这里照着造，顺便验证读取是有上限的。
		Body: &countingBody{
			Reader:  strings.NewReader(strings.Repeat("x", 1<<20)),
			onClose: func() { f.closed++ },
		},
	}, nil
}

// 东财的 82.push2delay 是 nginx 顶着一组后端，后端抖一下前面就是 502。
// 批量行情只有它一个源（tushare 的批量端点要 2000 积分），不重试的话
// 一次转瞬即逝的 502 就是整个 quotes 同步彻底失败。
func TestThrottleRetriesOnServerError(t *testing.T) {
	base := &statusRoundTripper{codes: []int{502, 503, 200}}
	tr, slept := newTestThrottle(base, 3)

	resp, err := tr.RoundTrip(newGetRequest(t))
	if err != nil {
		t.Fatalf("第三次应当成功: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("最终状态码应为 200，实际 %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	if base.calls != 3 {
		t.Fatalf("调用了 %d 次，期望 3 次", base.calls)
	}
	// 重试掉的那两个响应体必须被关掉，否则连接不回池，跑久了会越来越慢。
	if base.closed < 2 {
		t.Fatalf("被重试掉的响应体应当都被关闭，实际关了 %d 个", base.closed)
	}
	if len(*slept) != 2 || (*slept)[1] <= (*slept)[0] {
		t.Fatalf("退避时长应递增，实际 %v", *slept)
	}
}

// 重试用尽之后要把最后那个响应原样交回去，而不是吞成一个错误——
// provider 那边靠状态码拼出「eastmoney 返回 HTTP 502」这句话，
// 吞掉它会让排查时只剩一句没有上下文的重试失败。
func TestThrottleReturnsLastResponseAfterMaxAttempts(t *testing.T) {
	base := &statusRoundTripper{codes: []int{502, 502, 502, 502}}
	tr, _ := newTestThrottle(base, 3)

	resp, err := tr.RoundTrip(newGetRequest(t))
	if err != nil {
		t.Fatalf("应当把最后一个响应交回去而不是返回错误: %v", err)
	}
	if resp.StatusCode != 502 {
		t.Fatalf("最终状态码应为 502，实际 %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	if base.calls != 3 {
		t.Fatalf("调用了 %d 次，期望止于 3 次", base.calls)
	}
}

// 4xx 是我们自己的问题（参数错、没权限、找不到），重试一百次还是同样的结果。
func TestThrottleDoesNotRetryClientError(t *testing.T) {
	for _, code := range []int{400, 401, 403, 404, 408} {
		base := &statusRoundTripper{codes: []int{code, 200}}
		tr, _ := newTestThrottle(base, 3)

		resp, err := tr.RoundTrip(newGetRequest(t))
		if err != nil {
			t.Fatalf("HTTP %d 不该被当成传输错误: %v", code, err)
		}
		_ = resp.Body.Close()
		if base.calls != 1 {
			t.Fatalf("HTTP %d 不该重试，实际调用了 %d 次", code, base.calls)
		}
	}
}

// 只重试幂等方法：POST 在这几个源上都不是查询，重发有可能产生第二次副作用。
func TestThrottleDoesNotRetryNonIdempotentOnServerError(t *testing.T) {
	base := &statusRoundTripper{codes: []int{502, 200}}
	tr, _ := newTestThrottle(base, 3)

	req, err := http.NewRequest(http.MethodPost, "https://example.invalid/x", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("不该返回错误: %v", err)
	}
	_ = resp.Body.Close()
	if base.calls != 1 {
		t.Fatalf("POST 遇到 502 不该重发，实际调用了 %d 次", base.calls)
	}
}
