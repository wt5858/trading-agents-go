//go:build tools

// Package tools 把只在开发期用到的命令行工具钉进 go.mod。
//
// 没有这个文件，`go mod tidy` 会把 wire 当成未使用的依赖删掉，
// 于是下一个 clone 仓库的人跑 `make wire` 会拿到一个版本不确定的 wire——
// 而不同版本的 wire 生成的代码不完全一样，diff 里会出现一堆与本次改动无关的噪音。
// swag 同理：不同版本生成的 docs.go 在定义命名与排序上都有差异。
//
// # air 不在这里，这是有意的
//
// 热加载用的 air 钉在 Makefile 的 AIR 变量里，用 `go run pkg@version` 调。
// 放进这个文件意味着它的依赖闭包并进主模块：air v1.67 声明 go 1.26，
// `go get` 它会把本模块的 go 指令从 1.23 顶上去，并顺带升掉 wire、cobra、
// jwt、x/crypto——一个开发期的文件监听器不该有能力改动生产二进制的依赖版本。
//
// 这条边界的判据是「生成物是否进版本库」：wire 和 swag 的产物要提交、
// 必须锁版本保证人人生成的一致；air 不产出任何要提交的东西。
package tools

import (
	_ "github.com/google/wire/cmd/wire"
	_ "github.com/swaggo/swag/cmd/swag"
)
