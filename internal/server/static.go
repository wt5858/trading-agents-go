package server

import (
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/wt5858/trading-agents-go/internal/helpers/response"
)

// apiRoots 是「不属于前端」的路径前缀。
//
// 命中的未匹配请求要继续返回 JSON 信封的 404，而不是被 SPA 兜底成 index.html。
// 少了这道判断，一个拼错的接口路径会返回 200 + 一段 HTML，调用方表现为
// JSON.parse 失败——症状离真正的错误很远。
var apiRoots = []string{"/api", "/mcp", "/swagger", "/healthz"}

// isAPIPath 判断规范化之后的路径是否属于接口命名空间。两个细节都踩过：
//
// 调用方必须传规范化后的路径，否则 `//api/v1/x` 会绕过本判断拿到 index.html。
// 匹配用「精确相等或后跟 /」而非裸前缀，否则 /mcp-console、/healthzz 这类
// 前端路径会被一起吞掉。
func isAPIPath(p string) bool {
	for _, root := range apiRoots {
		if p == root || strings.HasPrefix(p, root+"/") {
			return true
		}
	}
	return false
}

// SPA 托管前端产物，并把找不到的前端路由兜底到 index.html。
//
// # 为什么这件事必须在 NoRoute 上做
//
// 前端用的是 history 路由：/analysis/tasks/xxx 在服务端并不存在对应文件。用户
// 在这个地址上刷新页面时，请求会走到这里——直接 404 的话，一个能正常点进去的
// 页面刷新一下就打不开了。兜底到 index.html 之后，前端路由自己会把地址解析出来。
//
// 代价是真正拼错的前端路径也会返回 index.html（然后由前端渲染 404 页），
// 这是 SPA 的固有取舍，不是缺陷。
func SPA(assets fs.FS, available bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 先规范化，后面所有判断都基于这一个值。
		// 分流与取文件用不同的路径是上一版的缺陷，见 isAPIPath 的注释。
		reqPath := path.Clean(c.Request.URL.Path)
		if !strings.HasPrefix(reqPath, "/") {
			reqPath = "/" + reqPath
		}

		if isAPIPath(reqPath) {
			response.FailWith(c, response.CodeNotFound, http.StatusNotFound, "接口不存在")
			return
		}

		// 前端没构建过就说清楚，别让人对着一个空白 404 猜。
		// 这条路径在开发期很常见：只跑了 make dev，没跑 make web-build。
		if !available {
			c.String(http.StatusNotFound,
				"前端尚未构建。在仓库根目录执行 make web-build 后重新编译，"+
					"或开发期改用 make web-dev（Vite :5173）。")
			return
		}

		// 只接受 GET/HEAD。前端资源没有写操作，一个 POST /foo 应该是 405 而不是
		// 悄悄返回一份 index.html。
		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
			response.FailWith(c, response.CodeNotFound, http.StatusNotFound, "接口不存在")
			return
		}

		name := strings.TrimPrefix(reqPath, "/")

		// 入口页一律走 serveIndex。"/" 解析出来就是 index.html，它在产物里真实存在，
		// 交给 ServeContent 能返回但少了禁缓存头——症状要到下次发版才显形（白屏）。
		if name == "" || name == "." || name == "index.html" {
			serveIndex(c, assets)
			return
		}

		f, err := assets.Open(name)
		if err != nil {
			// 不是磁盘上的文件，那就是一条前端路由，交给 index.html。
			serveIndex(c, assets)
			return
		}
		defer func() { _ = f.Close() }()

		info, err := f.Stat()
		if err != nil || info.IsDir() {
			serveIndex(c, assets)
			return
		}

		// assets/ 下的文件名里带内容哈希（Vite 生成），内容一变文件名就变，
		// 因此可以放心长缓存。其余文件（favicon 之类）不带哈希，不加这个头。
		if strings.HasPrefix(name, "assets/") {
			c.Header("Cache-Control", "public, max-age=31536000, immutable")
		}

		// comma-ok 而不是裸断言：embed.FS 的文件都实现了 Seek，今天恒成立，
		// 但换成别的 fs.FS（zip、远端流）时裸断言就是一个本可以避免的 panic。
		rs, ok := f.(io.ReadSeeker)
		if !ok {
			data, err := fs.ReadFile(assets, name)
			if err != nil {
				serveIndex(c, assets)
				return
			}
			c.Data(http.StatusOK, mimeOf(name), data)
			return
		}
		http.ServeContent(c.Writer, c.Request, info.Name(), info.ModTime(), rs)
	}
}

// mimeOf 仅供上面那条回退分支使用。ServeContent 自己会推断类型，
// 走不到它的时候才需要这个。
func mimeOf(name string) string {
	if t := mime.TypeByExtension(filepath.Ext(name)); t != "" {
		return t
	}
	return "application/octet-stream"
}

// serveIndex 写出 index.html，并明确禁止缓存。
//
// index.html 里引用的是带哈希的资源名，每次发版都会变。它自己要是被缓存住，
// 用户就会一直加载旧版本引用的、已经不存在的 JS——表现为发版后白屏，
// 且只有清缓存才能恢复。
func serveIndex(c *gin.Context, assets fs.FS) {
	data, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		response.FailWith(c, response.CodeNotFound, http.StatusNotFound, "页面不存在")
		return
	}
	c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
	c.Data(http.StatusOK, "text/html; charset=utf-8", data)
}
