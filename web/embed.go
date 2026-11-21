// Package web 只是前端静态资源的载体，通过 go:embed 打进二进制。
//
// 运行时不依赖 Node：M0 阶段用零构建的静态页，保证 go build 即可用；
// 前端框架（Vite + Vue）在 M1 接入后仍然产物落到 static/。
package web

import "embed"

//go:embed all:static
var FS embed.FS
