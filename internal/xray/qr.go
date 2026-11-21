package xray

import qrcode "github.com/skip2/go-qrcode"

// QRPNG 把分享链接渲染成 PNG 二维码。
//
// 放在服务端生成的原因：前端要保持零构建，引入二维码 JS 库
// 会让「一个 HTML + 一个 JS」的简单结构变成需要打包的工程。
func QRPNG(content string, size int) ([]byte, error) {
	if size < 128 || size > 1024 {
		size = 320
	}
	return qrcode.Encode(content, qrcode.Medium, size)
}
