// Package httpx 收敛出站 HTTP 的脱敏规则：URL 怎么写进日志、上游响应体怎么
// 拼进错误信息。
//
// 为什么要有这么一个包：这套规则原来在 llm 与 marketdata 各有一份逐字相同的拷贝。
// 两份拷贝对付「打日志」还算够用——改漏一边最多少脱敏一条日志。但错误信息会被
// 调用方打进日志，于是同一套规则出现了第三、第四个使用点，而脱敏规则一旦分叉，
// 分叉出去的那一份就是将来泄密的那一份。
//
// 本包只做字符串处理，不发请求、不认识任何 provider，因此谁都可以 import 它，
// 不会制造「行情数据源依赖大模型客户端」这种莫名其妙的方向。
package httpx

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Placeholder 是脱敏后留下的占位符。统一成一个值，便于在日志里搜。
const Placeholder = "***"

// secretPrefixes 是常见密钥的前缀。命中即认为整段是密钥。
var secretPrefixes = []string{"sk-", "sk_", "pk-", "pk_", "key-", "key_", "token-", "token_", "ghp_", "xoxb-"}

// minOpaqueLen 是「看起来像随机串」的长度门槛。
//
// 32 是权衡出来的：再短会误伤正常的路径段（模型名 gemini-2.0-flash 才 17 位），
// 再长会放过一批真实密钥（Finnhub 的 token 是 40 位十六进制，刚好在这条线之上）。
const minOpaqueLen = 32

// LooksLikeSecret 判断一段字符串是否像密钥。
//
// 判据有两条：命中已知前缀，或者「够长且只由密钥字符集构成」。
// 第二条会误伤——一个 32 位以上的纯字母数字路径段会被打码，但把一段正常路径
// 误打成 *** 只是让日志难读一点，反过来漏掉一个密钥是不可逆的。
func LooksLikeSecret(s string) bool {
	if s == "" {
		return false
	}
	lower := strings.ToLower(s)
	for _, p := range secretPrefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	if len(s) < minOpaqueLen {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// RedactPath 把 URL 路径里像密钥的段替换成占位符。
//
// 自建网关有 /v1/<key>/chat/completions 这种形态，而 baseURL 是运维配置给的，
// 这一层管不住调用方怎么写。
func RedactPath(path string) string {
	if !strings.Contains(path, "/") {
		return path
	}
	segs := strings.Split(path, "/")
	hit := false
	for i, s := range segs {
		if LooksLikeSecret(s) {
			segs[i] = Placeholder
			hit = true
		}
	}
	if !hit {
		return path
	}
	return strings.Join(segs, "/")
}

// RedactURL 把 URL 压成可以安全落日志的形式：scheme://host/path，
// query 整段丢弃，path 再过一遍 RedactPath。
//
// query 一律不要，而不是挑几个安全的留下：这一层无从判断某个参数是不是密钥，
// 而把密钥放在 query string 里是相当常见的写法。userinfo 一并丢掉——
// https://key@gateway/... 同样是泄密。
func RedactURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	if u.Host == "" {
		// 相对 URL（少见，但 http.NewRequest 允许），只有路径可以处理。
		return RedactPath(u.Path)
	}
	scheme := u.Scheme
	if scheme == "" {
		scheme = "http"
	}
	return scheme + "://" + u.Host + RedactPath(u.Path)
}

// RedactError 把错误链里的 *url.Error 换成不含 query 的版本。
//
// net/http 的 Client.Do 失败时返回的 *url.Error 带着**完整**请求 URL——包括
// query string。调用方通常会把这个错误 Wrap 之后原样打进日志，于是凡是用
// ?token= 认证的上游，一次连接超时就等于把密钥写进日志文件。
//
// 不是 *url.Error 就原样返回：这里只认这一种已知会携带 URL 的错误类型，
// 不去猜别的错误信息里有没有敏感内容。
func RedactError(err error) error {
	if err == nil {
		return nil
	}
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	safe := ue.URL
	if u, perr := url.Parse(ue.URL); perr == nil {
		safe = RedactURL(u)
	} else {
		// 连解析都失败的 URL 不猜，整段丢掉——它已经不可能是有用的排查线索了。
		safe = Placeholder
	}
	// 重建而不是改字段：*url.Error 是调用方可能 errors.As 出来看的类型，
	// 保留它的形状，只换掉 URL 那一个字段的内容。
	return &url.Error{Op: ue.Op, URL: safe, Err: ue.Err}
}

// SanitizeBody 把上游响应体处理成可以安全拼进错误信息的字符串。
//
// 上游的错误响应是排查时最有价值的线索（「余额不足」「模型不存在」这类信息只在
// 里面），所以不能整段丢掉。但它同时是**上游完全可控**的内容，而部分网关会把收到的
// Authorization 头原样回显在错误体里——那就等于我们自己把密钥抄进了日志。
//
// 于是分两步：
//
//  1. 逐字替换掉 known 里的值。这一步是确定的，不靠猜：能被回显的密钥只可能是
//     我们自己发出去的那一个，拿它做精确匹配就能把「回显」这个场景堵死。
//  2. 再按通用形态扫一遍，把 Bearer xxx 与像密钥的词替换掉。这一步是兜底，
//     防的是上游泄漏**别人的**密钥，只能靠形态识别，必然有漏网。
//
// 第二步不是安全边界，别把它当成。真正的保证来自第一步，以及「绝不打请求体、
// 绝不打 header」这两条在调用点上的纪律。
func SanitizeBody(body []byte, limit int, known ...string) string {
	s := strings.TrimSpace(string(body))
	if s == "" {
		return ""
	}

	// 先替换已知密钥，再截断。顺序反了的话，一个正好跨在截断点上的密钥
	// 会被切成两半，前半段留在字符串里，而它仍然是密钥的一部分。
	for _, k := range known {
		if len(k) < 8 {
			// 太短的值不做替换：一个三四位的 token 拿去全文替换，
			// 会把响应里正常的文字也打得七零八落。
			continue
		}
		s = strings.ReplaceAll(s, k, Placeholder)
	}
	s = scrubSecretLike(s)

	if limit > 0 && len(s) > limit {
		s = s[:limit] + "...(truncated)"
	}
	return s
}

// scrubSecretLike 按形态替换 Bearer 令牌与像密钥的词。
//
// 按空白与常见 JSON 标点切词，逐词判断。这么做会漏掉嵌在长串里、
// 两侧没有分隔符的密钥——这是形态识别的固有局限，不是实现没写好。
func scrubSecretLike(s string) string {
	var b strings.Builder
	b.Grow(len(s))

	word := make([]rune, 0, 64)
	flush := func() {
		if len(word) == 0 {
			return
		}
		w := string(word)
		if LooksLikeSecret(w) {
			b.WriteString(Placeholder)
		} else {
			b.WriteString(w)
		}
		word = word[:0]
	}

	for _, r := range s {
		if isWordBoundary(r) {
			flush()
			b.WriteRune(r)
			continue
		}
		word = append(word, r)
	}
	flush()

	return stripBearer(b.String())
}

func isWordBoundary(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', '"', '\'', ',', ':', '{', '}', '[', ']', '(', ')', '=', ';', '<', '>':
		return true
	}
	return false
}

// stripBearer 处理 "Bearer xxx" 这种两段式：xxx 本身未必命中 LooksLikeSecret
// （短令牌、含点的 JWT 都不会），但 Bearer 后面跟的东西一定是凭证。
//
// 实现上逐段消费、只向前推进，绝不回头重扫已处理的部分。
// 这不是风格选择：替换进去的占位符本身不含分隔符，原地重扫会把它当成新的令牌
// 一替再替，每轮结果都和上一轮相同——一个不会退出的循环。
func stripBearer(s string) string {
	const marker = "Bearer "

	var b strings.Builder
	b.Grow(len(s))

	rest := s
	for {
		i := strings.Index(rest, marker)
		if i < 0 {
			b.WriteString(rest)
			return b.String()
		}

		// marker 连同它前面的内容一起定稿，后面不再参与查找。
		b.WriteString(rest[:i+len(marker)])
		tail := rest[i+len(marker):]

		end := strings.IndexFunc(tail, isWordBoundary)
		if end < 0 {
			end = len(tail)
		}
		if end > 0 {
			b.WriteString(Placeholder)
		}
		// end == 0 表示 "Bearer " 后面直接是分隔符，没有令牌可脱，什么都不写。
		rest = tail[end:]
	}
}

// Errorf 是给调用点用的小工具：按格式拼错误，但先把 *url.Error 脱敏。
//
// 存在的意义是让调用点少写一次 RedactError——漏写不会有任何编译或测试信号。
func Errorf(format string, args ...any) error {
	for i, a := range args {
		if err, ok := a.(error); ok {
			args[i] = RedactError(err)
		}
	}
	return fmt.Errorf(format, args...)
}
