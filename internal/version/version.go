// Package version 保存构建期注入的版本信息。
package version

var (
	// Version 语义化版本号，发布时由 -ldflags 覆盖。
	Version = "0.3.14-dev"
	// Commit 构建对应的 git 提交。
	Commit = "none"
	// BuildAt 构建时间（UTC）。
	BuildAt = "unknown"
)
