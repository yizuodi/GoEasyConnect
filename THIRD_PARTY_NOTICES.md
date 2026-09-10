# Third-Party Notices

Go dependencies are pinned in `go.mod` and `go.sum`. The direct dependencies at
the time of this release are:

| Dependency | Version | License |
| --- | --- | --- |
| github.com/coder/websocket | v1.8.14 | ISC |
| github.com/creack/pty | v1.1.24 | MIT |
| github.com/google/uuid | v1.6.0 | BSD-3-Clause |
| github.com/pelletier/go-toml/v2 | v2.2.4 | MIT |
| modernc.org/sqlite | v1.39.1 | BSD-3-Clause |

Their complete license texts are included in their respective source modules.
Transitive dependency versions and checksums are recorded in `go.mod` and
`go.sum`.

The embedded terminal frontend includes `@xterm/xterm` v6.0.0 and
`@xterm/addon-fit` v0.11.0, distributed under the MIT License:

```text
Copyright (c) 2014-2024 The xterm.js authors. All rights reserved.

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```
