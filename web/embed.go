// Package web 把前端构建产物嵌进二进制。
//
// # 这个文件为什么在 web/ 而不是 internal/server/
//
// go:embed 的路径模式不允许出现 ..，只能嵌入本包所在目录及其子目录。前端产物在
// web/dist，所以嵌它的 Go 文件只能放在 web/。把它挪进 internal/server 再写
// ../../web/dist 是编译期错误，不是风格问题。
//
// 本包只有这一个文件，且不依赖仓库里任何其他包——它是被 internal/server 单向依赖的
// 叶子节点。
package web

import (
	"embed"
	"io/fs"
)

// distFS 是 `npm run build` 的产物。
//
// all: 前缀是必需的：不带它时 go:embed 会跳过以 . 和 _ 开头的文件，而仓库里
// 提交的占位文件恰好叫 dist/.gitkeep——跳过它就等于这个模式一个文件都匹配不上，
// 编译直接失败（"pattern dist: no matching files found"）。
//
// 那个占位文件的存在理由见 web/.gitignore 里的说明：产物本身不进版本库，
// 而 go:embed 又要求目录非空，两者只能靠它调和。
//
//go:embed all:dist
var distFS embed.FS

// Assets 返回可直接托管的前端产物，以及它是否真的被构建过。
//
// 第二个返回值不是多余的防御。仓库里只提交了 dist/.gitkeep，任何人 clone 下来
// 直接 go build 都能编过，但拿到的二进制里没有 index.html。让调用方知道这件事，
// 才能给出「前端没构建，跑 make web-build」这种能照着做的提示，而不是一个 404。
func Assets() (fs.FS, bool) {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return nil, false
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil, false
	}
	return sub, true
}
