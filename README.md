# GoEasyConnect

[![CI](https://github.com/yizuodi/GoEasyConnect/actions/workflows/ci.yml/badge.svg)](https://github.com/yizuodi/GoEasyConnect/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

GoEasyConnect 是一个面向小型、自托管、单机环境的 Claude Code 与 Codex Web
管理面板。桌面端、移动端和 xterm.js 通过 `go:embed` 编译进一个静态 Go
二进制；运行时只需要二进制、`config.json`、SQLite 数据库和错误日志。

> [!WARNING]
> GoEasyConnect 能以服务账户权限启动 AI CLI 并提供交互式终端。请将它视为管理界面：使用强密码，只通过 HTTPS 反向代理对外提供服务，并谨慎启用跳过权限确认。

## 功能

- Claude Code 与 Codex 会话创建、启动、停止、恢复及终端访问
- 桌面端和移动端界面，支持 WebSocket 与 HTTP 轮询
- 可选的 Claude Code/Codex 对话模式，保留原生终端作为回退
- Claude JSON 和 Codex TOML 配置档
- Codex 自定义 OpenAI-compatible Provider，无需执行 `codex login`
- API Key 仅注入目标进程，不写入 Codex TOML、命令参数或错误日志
- 兼容 EasyClaude/EasyConnect SQLite Schema，并自动执行增量迁移
- SQLite WAL、`0600` 敏感文件权限和有大小上限的 `error.log`
- 有界终端、轮询和 WebSocket 缓冲，适合资源有限的单机环境
- Claude Code/Codex 自动继续：按周期检测任务是否结束，空闲时限次发送“继续”
- 桌面端和移动端可检查 GitHub 最新 Release，并通过受限更新服务一键升级

## 系统要求

- Linux（PTY 和进程组管理依赖 Linux/Unix 行为）
- Go 1.25 或更新版本（仅源码构建时需要）
- 已安装并配置至少一个 CLI：`claude` 或 `codex`
- 使用 `runAsUser` 时，需要 `sudo` 和对应的免交互执行权限

## 一键安装

首个安装脚本只支持 Debian 的 `amd64`/`arm64`。它会从最新 GitHub Release
下载对应架构、校验 SHA-256，引导监听地址、端口、访问密码、安装目录、默认
Agent、CLI 路径、工作目录和 AI 会话 sudo 提权选项，创建或复用执行用户，
并安装 systemd 服务：

```bash
curl -fsSL https://raw.githubusercontent.com/yizuodi/GoEasyConnect/main/scripts/install.sh | sudo bash
```

脚本会保留既有 `config.json`、SQLite 数据和日志；确认重新配置时会先生成
带时间戳的备份。AI 会话 sudo 提权默认开启；这会让掌握 Web 访问密码或 AI
会话的用户具备系统级修改能力，请仅在信任环境中启用。

### 升级

通过安装脚本部署的实例可以显式执行无交互升级：

```bash
curl -fsSL https://raw.githubusercontent.com/yizuodi/GoEasyConnect/main/scripts/install.sh | sudo bash -s -- upgrade
```

升级器从 `easyconnect.service` 自动识别二进制、配置和运行用户，下载并校验最新
Release，然后停止服务并将原二进制、配置、systemd unit 及 SQLite 数据库备份到
`/var/backups/easyconnect/`。新版本会先执行配置和数据库检查；检查失败或原先处于
运行状态的服务无法重新启动时，升级器自动恢复原二进制和数据库。原服务升级前
未运行时，升级完成后仍保持未运行。自定义 unit 中的 `ExecStart` 路径不能包含空格。

网页一键更新支持由安装脚本安装的实例；手工复制二进制并使用示例 systemd unit
时不会自动安装 root 更新助手，应继续使用本节的命令行升级方式。

通过安装脚本安装或升级后，页面顶部的“更新”按钮也可以检查和安装最新正式
Release。网页进程不会直接取得 root shell；它只能启动固定的
`goeasyconnect-updater.service`。更新器仅从本项目 GitHub Release 下载当前架构的
包，校验 `checksums.txt`，备份二进制、配置与 SQLite 文件，并在检查或启动失败时
自动回滚。更新过程中页面会短暂断开。可在 `config.json` 中设置
`"updates": {"enabled": false}` 关闭网页更新入口。

## 构建与运行

项目不需要 Node.js、npm 或 CGO：

```bash
git clone https://github.com/yizuodi/GoEasyConnect.git
cd GoEasyConnect
CGO_ENABLED=0 go test -buildvcs=false ./...
make build

cp config.example.json config.json
# 修改 auth.password，并按需配置 CLI、用户和工作目录。
chmod 600 config.json
./easyconnect --check --config ./config.json
./easyconnect --config ./config.json
```

默认监听 `127.0.0.1:26890`。桌面页面为 `/`，移动页面为 `/mobile`。
未传 `--config` 时，程序读取二进制所在目录的 `config.json`；也可以设置
`EASYCONNECT_CONFIG`。配置中的相对路径均以配置文件所在目录为基准。

示例密码 `change-this-password` 和空密码会被拒绝。可生成随机密码：

```bash
openssl rand -base64 32
```

## 配置 Claude Code 与 Codex

`claude.binary` 和 `codex.binary` 可以是 PATH 中的命令，也可以是绝对路径。
留空的 home/settings/session 路径会根据服务用户自动推导。推荐让 Web 服务和 CLI
使用同一个专用账户；需要以其他账户启动时设置对应的 `runAsUser`。

在 Web 页面中创建配置档：

- Claude 使用合法的 `settings.json` 内容，例如 `env.ANTHROPIC_AUTH_TOKEN`。
- Codex 可以保存原生 TOML；自定义 Provider 还可使用顶层 `api_key`、`base_url`、
  `model` 和 `wire_api` 简写。凭据简写仅保存在受保护的 SQLite 数据库中，生成给
  Codex 的配置文件会移除这些字段。

完整设置及安全占位值见 [`config.example.json`](config.example.json)。

### 对话模式

在 `config.json` 中显式开启后，新建或已停止的 Claude Code/Codex 会话可以选择“原生终端”或
“对话模式”作为启动模式：

```json
"experimental": {
  "conversationMode": true
}
```

Codex 对话模式启动常驻 `codex app-server`；Claude Code 对话模式使用官方
`stream-json` 输入输出协议。两者都会保存结构化消息、命令和文件变更事件；原生
终端模式仍直接运行官方 TUI。两种模式沿用各代理的原生 session/thread ID，因此
停止后切换模式仍能继续已提交的历史，但不会同步两侧未发送的草稿。模式在运行中
锁定，必须先停止会话才能切换并重新启动。

对话模式启动前必须启用 `Skip Perms`；这等同于允许代理无需审批执行命令和修改
文件，请只在受信任的单用户环境中使用。停止对话中的回复只会中断当前 turn，不会
关闭常驻进程；顶部“停止”才会结束整个会话。

对话页面首次只加载最近 50 条消息及关联工具摘要，点击“加载更早消息”可继续
向前分页。后续轮询仅返回新增、修改或删除的消息，不重复下载已读历史。单条
消息默认显示最多 4 KiB 的预览，点击“展开完整消息”读取正文；工具输出和文件
变更详情在展开卡片时加载。完整历史仍保存在数据库，原生 session/thread ID 不变。
单页工具摘要超过 100 条时可点击“加载更多工具记录”继续读取。
页面最多缓存 200 条消息和 400 条工具摘要，完整记录仍保存在服务器。阅读更早
历史时新增消息只显示提示，可点击“回到最新”恢复实时窗口。执行中每秒检查，
空闲时每 5 秒检查，后台标签页每 30 秒检查，回到前台立即刷新；失败请求指数退避。
支持 gzip 的客户端会自动接收压缩后的对话响应。消息变更游标只保存每条消息的
最新版本，不会随输出片段持续增加记录数量。
超过缓存上限的工具记录仍可分页查看；查看工具历史后点击“回到最新”恢复实时
工具窗口。向上阅读时页面以当前可见消息为滚动锚点，避免输出更新或缓存淘汰
导致阅读位置跳动。

### 自动继续

Claude Code/Codex 会话顶部的“自动继续”按钮可以开启服务端自动继续。每次开启可设置触发次数和
检查周期，周期默认 20 分钟；达到触发次数后自动关闭，也可随时手动关闭。只有
会话进程仍在运行且当前任务已经结束时才会发送“继续”，执行中的检查不会消耗次数。
对话模式使用常驻进程的 turn 状态；原生终端模式读取代理原生会话 JSONL 的任务或
`stop_reason` 状态。状态无法确认时不会发送。会话停止后暂停检测，重新启动后从一个
完整周期重新计时。

## systemd 部署

先创建专用用户和部署目录，再安装二进制及配置：

```bash
sudo useradd --system --home-dir /srv/easyconnect --shell /usr/sbin/nologin easyconnect
sudo install -d -o easyconnect -g easyconnect -m 700 /srv/easyconnect
sudo install -o easyconnect -g easyconnect -m 755 easyconnect /srv/easyconnect/easyconnect
sudo install -o easyconnect -g easyconnect -m 600 config.json /srv/easyconnect/config.json
sudo install -m 644 easyconnect.service.example /etc/systemd/system/easyconnect.service
sudo systemctl daemon-reload
sudo systemctl enable --now easyconnect
```

示例 unit 使用了 systemd 文件系统保护。若 CLI 或工作目录位于
`/srv/easyconnect` 之外，请按最小权限原则调整 `ReadWritePaths`、`ProtectHome` 和
服务账户权限。若确实需要通过 `runAsUser` 调用 `sudo`，还需要审慎移除
`NoNewPrivileges=true` 并配置仅允许目标 CLI 的最小化 sudoers 规则；更推荐直接让
服务以 CLI 所属专用账户运行。

### HTTPS 反向代理

建议保持回环地址监听，由 Caddy、Nginx 等代理提供 TLS。Nginx 需要传递 WebSocket
升级头，例如：

```nginx
location / {
    proxy_pass http://127.0.0.1:26890;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
}
```

若确需直接监听局域网，可将 `server.host` 改为 `0.0.0.0`，但不要在不受信任网络
上使用明文 HTTP。WebSocket 使用短时一次性 ticket，管理密码不会出现在连接 URL。

## 从 Node.js EasyConnect 迁移

Go 版可直接使用现有 `config.json` 和数据库。升级前不要让 Node 和 Go 进程同时
打开同一数据库或管理同一会话。

1. 停止活跃会话并确认恢复 ID 已保存。
2. 停止 Node 服务。
3. 备份 `config.json`、数据库及其 `-wal`/`-shm` 文件，或使用 SQLite Online Backup。
4. 放入 Go 二进制并保留现有配置；确保 `auth.password` 非空。
5. 执行 `./easyconnect --check --config ./config.json` 后再启动服务。
6. 验证桌面端、移动端以及 Claude/Codex 会话。

未配置 `database.path` 时，如果配置目录存在 `easyclaude.db`，程序会继续使用它；
否则创建 `easyconnect.db`。启动时会补齐旧 Schema 字段，并将遗留的 `running`
状态重置为 `stopped`。

## 运行文件与备份

典型目录如下；这些内容已被 `.gitignore` 排除，不应提交到 Git：

```text
/srv/easyconnect/
├── easyconnect
├── config.json
├── easyconnect.db
├── easyconnect.db-wal
├── easyconnect.db-shm
└── error.log
```

停止服务后备份整个目录，或在线备份 SQLite。`error.log` 只记录服务内部错误，设计
上不记录 API Key、登录密码、用户输入或终端输出。

## 开发

```bash
make test
make vet
make build
node --check web/app.js
node --check web/conversation-feed.js
node --test scripts/conversation_feed_test.cjs
```

后端入口与生命周期位于 `main.go`/`app.go`，SQLite 逻辑位于 `store.go`，PTY 与
会话恢复位于 `session.go`，HTTP/WebSocket 位于 `http*.go`/`websocket.go`，前端在
`web/`。贡献前请阅读 [`CONTRIBUTING.md`](CONTRIBUTING.md)，安全问题请按
[`SECURITY.md`](SECURITY.md) 私下报告。

## 自动构建与发布

每次 Push 和 Pull Request 都会运行格式、测试、竞态检测、静态分析、漏洞扫描，
并上传一个保留 14 天的 Linux amd64 构建产物。推送以 `v` 开头的标签会构建 Linux
amd64/arm64 压缩包、生成 `checksums.txt`，并自动创建 GitHub Release：

```bash
git tag -a v0.1.0 -m "GoEasyConnect v0.1.0"
git push origin v0.1.0
```

Release 使用仓库自动提供的 `GITHUB_TOKEN`，不需要保存个人访问令牌。

## License

本项目采用 [MIT License](LICENSE)。嵌入前端依赖的许可信息见
[`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md)。Claude、Claude Code、Codex
和 OpenAI 是其各自所有者的商标；本项目是独立的开源项目，不代表官方认可。
