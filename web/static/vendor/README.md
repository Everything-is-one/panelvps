# 第三方前端组件

面板零构建，这些文件直接以静态资源形式随二进制内嵌，不走 npm。

| 文件 | 来源 | 版本 | 许可 |
|---|---|---|---|
| `xterm.js` | [@xterm/xterm](https://www.npmjs.com/package/@xterm/xterm) | 5.5.0 | MIT |
| `xterm.css` | 同上 | 5.5.0 | MIT |
| `addon-fit.js` | [@xterm/addon-fit](https://www.npmjs.com/package/@xterm/addon-fit) | 0.10.0 | MIT |

升级方式：

```bash
curl -fsSL https://cdn.jsdelivr.net/npm/@xterm/xterm@5.5.0/lib/xterm.js     -o xterm.js
curl -fsSL https://cdn.jsdelivr.net/npm/@xterm/xterm@5.5.0/css/xterm.css    -o xterm.css
curl -fsSL https://cdn.jsdelivr.net/npm/@xterm/addon-fit@0.10.0/lib/addon-fit.js -o addon-fit.js
```

```

Copyright (c) 2014 The xterm.js authors. All rights reserved.

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
```
