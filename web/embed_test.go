package web

import (
	"io/fs"
	"strings"
	"testing"
)

// TestAssetsReflectsBuildState 钉住 Assets 的两个返回值是一致的。
//
// 这个测试在两种环境下都必须过，而且断言的东西不同：
//
//   - 刚 clone、没跑过 make web-build：dist 里只有占位文件，available 必须是 false。
//     这条保证了「前端没构建」不会表现为一个 200 返回空白页。
//   - 跑过构建：必须能读到 index.html，且里面确实引用了带哈希的资源。
//     这条保证了 go:embed 收的是真产物，而不是又一个占位文件。
//
// 不写成「要求前端必须构建过」，是因为那样会让 `go test ./...` 在 CI 里
// 强依赖 node 工具链——而后端改动跑测试时不该被前端卡住。
func TestAssetsReflectsBuildState(t *testing.T) {
	assets, available := Assets()

	if !available {
		if assets != nil {
			t.Error("available 为 false 时 assets 应为 nil，否则调用方可能误用")
		}
		t.Skip("前端未构建（dist 里只有占位文件）——跑 make web-build 后本测试会断言真实产物")
	}

	data, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		t.Fatalf("available 为 true 但读不到 index.html: %v", err)
	}

	// Vite 产出的 index.html 一定会引用 /assets/ 下带哈希的入口脚本。
	// 读到的要是那个手写的开发期 index.html（引用 /src/main.tsx），
	// 说明 dist 里装的是源码模板而不是构建产物。
	if !strings.Contains(string(data), "/assets/") {
		t.Errorf("index.html 里没有 /assets/ 引用，收进来的可能不是构建产物:\n%s", data)
	}
	if strings.Contains(string(data), "/src/main.tsx") {
		t.Error("index.html 引用了 /src/main.tsx，这是开发期模板，不是构建产物")
	}

	// 入口脚本本身也要在。只有 index.html 而没有 assets 的话，页面打开就是白屏。
	entries, err := fs.ReadDir(assets, "assets")
	if err != nil {
		t.Fatalf("产物里没有 assets 目录: %v", err)
	}
	if len(entries) == 0 {
		t.Error("assets 目录是空的")
	}
}
