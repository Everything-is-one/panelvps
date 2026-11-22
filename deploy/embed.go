// Package deploy 把部署用的 systemd 单元编进二进制。
//
// 为什么需要：自更新只替换二进制（/opt/panelvps/bin/panelvps），不碰
// /etc/systemd/system/panelvps.service。而单元里装的是沙箱策略
// （ProtectSystem / ReadOnlyPaths / CapabilityBoundingSet），它一变，
// 老单元就会挡住新版本的功能 —— v0.3.5 把 /usr 从只读改成可写、
// v0.4.6 干脆翻成「默认放开、只读点名」，通过「设置 → 版本更新」升级的人
// 都拿不到新单元，装软件依旧报 read-only file system。
//
// 所以把单元作为一份可自检的资源编进来：面板能比对磁盘上的单元，
// 不一致时提示并支持一键同步（写文件 + daemon-reload，重启后生效）。
package deploy

import _ "embed"

// ServiceUnit 是主服务单元的内容，与同目录的 panelvps.service 同一份，
// install.sh 也读这个文件，不存在两份拷贝漂移的问题。
//
//go:embed panelvps.service
var ServiceUnit string
