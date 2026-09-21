// Package migrations 以 embed 的方式携带全部 goose 迁移脚本。
//
// 编译进二进制而不是运行时读目录：容器镜像里只有一个可执行文件，
// 迁移脚本和它跑的那份代码永远是同一次构建产出的，不会出现
// 「代码更新了、挂载的 SQL 目录还是旧的」这种最难排查的错位。
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
