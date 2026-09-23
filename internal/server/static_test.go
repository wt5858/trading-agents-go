package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/gin-gonic/gin"
)

// fakeAssets 模拟一份构建好的前端产物：一个入口页加一个带哈希的资源。
func fakeAssets() fstest.MapFS {
	return fstest.MapFS{
		"index.html":             {Data: []byte("<!doctype html><div id=root></div>")},
		"assets/index-abc123.js": {Data: []byte("console.log(1)")},
	}
}

func newSPAEngine(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.NoRoute(SPA(fakeAssets(), true))
	return engine
}

func do(t *testing.T, engine *gin.Engine, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

// TestSPAKeepsAPIPathsAsJSON 钉住这次改动的核心风险。
//
// NoRoute 从「一律返回 JSON 404」改成了「兜底到 index.html」。如果分流判断写错，
// 一个拼错的接口路径会返回 200 + 一段 HTML，调用方那边表现为 JSON 解析失败——
// 症状离真正的错误很远，值得用测试钉死。
func TestSPAKeepsAPIPathsAsJSON(t *testing.T) {
	engine := newSPAEngine(t)

	for _, target := range []string{
		"/api/v1/not-exist",
		"/swagger/nope",
		"/healthz/extra",
	} {
		rec := do(t, engine, http.MethodGet, target)

		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: 状态码 = %d, 期望 404", target, rec.Code)
		}
		var envelope struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
			t.Errorf("%s: 响应不是 JSON 信封: %v (body=%q)", target, err, rec.Body.String())
			continue
		}
		if envelope.Code == 0 {
			t.Errorf("%s: 信封 code = 0，期望一个失败码", target)
		}
	}
}

// TestSPAFallsBackToIndex 前端的 history 路由在服务端没有对应文件，
// 刷新页面时必须拿到 index.html，否则「能点进去的页面刷新就打不开」。
func TestSPAFallsBackToIndex(t *testing.T) {
	engine := newSPAEngine(t)

	for _, target := range []string{"/", "/analysis", "/analysis/tasks/task_123"} {
		rec := do(t, engine, http.MethodGet, target)

		if rec.Code != http.StatusOK {
			t.Errorf("%s: 状态码 = %d, 期望 200", target, rec.Code)
		}
		if got := rec.Body.String(); got != "<!doctype html><div id=root></div>" {
			t.Errorf("%s: 返回的不是 index.html: %q", target, got)
		}
		// index.html 被缓存住的话，发版后用户会一直加载已经不存在的旧 JS，表现为白屏。
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache, no-store, must-revalidate" {
			t.Errorf("%s: Cache-Control = %q, 期望禁止缓存", target, cc)
		}
	}
}

// TestSPAServesHashedAssetWithLongCache 带内容哈希的资源改名即改内容，可以长缓存。
func TestSPAServesHashedAssetWithLongCache(t *testing.T) {
	engine := newSPAEngine(t)

	rec := do(t, engine, http.MethodGet, "/assets/index-abc123.js")

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", rec.Code)
	}
	if rec.Body.String() != "console.log(1)" {
		t.Errorf("返回的不是那个资源文件: %q", rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q, 期望长缓存", cc)
	}
}

// TestSPARejectsNonGET 前端资源没有写操作。一个 POST /foo 不该悄悄拿到 index.html。
func TestSPARejectsNonGET(t *testing.T) {
	engine := newSPAEngine(t)

	rec := do(t, engine, http.MethodPost, "/analysis")

	if rec.Code != http.StatusNotFound {
		t.Errorf("状态码 = %d, 期望 404", rec.Code)
	}
	if body := rec.Body.String(); body == "<!doctype html><div id=root></div>" {
		t.Error("POST 请求拿到了 index.html")
	}
}

// TestSPAWithoutBuiltFrontend 只跑了 make dev、没跑 make web-build 时的表现。
// 这条路径在开发期很常见，提示必须是能照着做的，而不是一个空白 404。
func TestSPAWithoutBuiltFrontend(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.NoRoute(SPA(nil, false))

	rec := do(t, engine, http.MethodGet, "/analysis")
	if rec.Code != http.StatusNotFound {
		t.Errorf("状态码 = %d, 期望 404", rec.Code)
	}
	if body := rec.Body.String(); body == "" {
		t.Error("前端未构建时应给出可操作的提示，实际是空响应")
	}

	// 前端没构建不该影响接口路径的行为。
	rec = do(t, engine, http.MethodGet, "/api/v1/nope")
	if rec.Code != http.StatusNotFound {
		t.Errorf("接口路径状态码 = %d, 期望 404", rec.Code)
	}
	if rec.Header().Get("Content-Type") == "text/plain; charset=utf-8" {
		t.Error("接口路径返回了纯文本，期望 JSON 信封")
	}
}
