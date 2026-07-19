# ish — 交互式 Shell 助手

[简体中文](README.zh-CN.md) | [English](README.md)

`ish` 为 AI agent 驱动的自动化渗透测试管理多个后台 shell 上下文。每个
上下文是一个长驻子进程（如 `nc -lvvp 4444`、`ssh host`、`evil-winrm ...`、
sudo 密码提示），其 stdin/stdout/stderr 由 daemon 缓存。一次性 AI 客户端
可以跨多次调用向前者喂输入、从后者读输出——让 agent 能"一次一条命令"地
驱动交互式 shell。

daemon 监听 **Unix Domain Socket**（Linux/macOS）或 **Named Pipe**
（Windows），仅限本机当前用户访问。

---

## 为什么需要

AI agent 自动化渗透时，通常一次只能执行一条 shell 命令。一旦反弹 shell
到手（`nc -lvvp 4444` 返回），或 `ssh`/`evil-winrm` 启动后，agent 就
失去了与会话交互的能力——它只能发一次性命令。

`ish` 把每个会话寄存在 daemon 管理的上下文里。agent 创建上下文、发输入、
读输出、继续下一步——全都是短命 CLI 调用。人类也可以像 `screen -r` 一样
`attach` 到任意上下文进行手动操作。

---

## 编译

需要 Go 1.22+。

```bash
# 当前平台
go build -o bin/ish .

# Linux（交叉编译，无需 CGO）
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/ish-linux-amd64 .

# Windows（交叉编译）
GOOS=windows GOARCH=amd64 go build -o bin/ish.exe .

# macOS——需在 macOS 主机上编译。从 Linux/Windows 交叉编译不支持，
# 因为 term_unix.go 用的是 Linux termios 常量（TCGETS/TCSETS），
# macOS 需要 TIOCGETA/TIOCSETA。
GOOS=darwin GOARCH=amd64 go build -o bin/ish-darwin-amd64 .
```

二进制自包含：同一个可执行文件既是客户端也是 daemon（daemon 首次使用
时自动启动，也可显式执行 `ish daemon`）。

### 依赖

| 平台 | PTY 后端 | IPC 后端 |
|------|----------|----------|
| Linux / macOS | `github.com/creack/pty` | Unix Domain Socket |
| Windows | ConPTY via `golang.org/x/sys/windows`（Win10 1809+） | Named Pipe（`Microsoft/go-winio`） |

Windows 上不使用任何第三方 ConPTY 封装；代码直接调用
`CreatePseudoConsole` / `STARTUPINFOEX` / `PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE`。

---

## 快速开始

```bash
# 1. 启动一个反弹 shell 监听器；捕获其 uid（打印到 stdout）
ish new --name revshell -- nc -lvvp 4444
# -> 9a3f1c2d8e7b6a50

# 2. 一旦有 shell 连回，发送命令
ish input --uid revshell "id; whoami; uname -a"

# 3. 读取新输出（持续流式拉取最多 5 秒）
ish stdout --uid revshell --offset 0 --follow --timeout 5

# 4. 需要手动操作时交互式 attach（Ctrl-Q 分离）
ish attach --uid revshell

# 5. 收尾
ish kill --uid revshell
ish rm   --uid revshell
```

---

## 命令

| 命令 | 用途 |
|------|------|
| `new`     | 启动新的后台上下文 |
| `ls`      | 列出所有上下文 |
| `info`    | 查看某个上下文详情 |
| `stdout`  | 读取 stdout（原始字节 + offset 元数据） |
| `stderr`  | 读取 stderr（pty 模式下与 stdout 镜像） |
| `input`   | 向上下文 stdin 发送字节 |
| `attach`  | 交互式挂载终端（类似 `screen -r`） |
| `wait`    | 阻塞等待上下文退出（或超时） |
| `kill`    | 杀掉上下文的进程树 |
| `rm`      | 移除上下文（仍在运行则先 kill） |
| `clean`   | 清理已退出的上下文（`--all` 连运行中一起清） |
| `daemon`  | 显式启动 daemon |
| `shutdown`| 停止 daemon |
| `version` | 打印版本号 |
| `help`    | 显示完整帮助 |

运行 `ish help` 或 `ish <命令> -h` 查看权威 flag 列表。

---

## PTY 模式 vs 管道模式

| 模式 | flag | 适用场景 |
|------|------|----------|
| **管道**（默认） | `--no-pty` | 反弹 shell（`nc`）、`evil-winrm`、任何不需要 TTY 的程序。stdout 与 stderr 是分离的两条流。 |
| **PTY** | `--pty` | `sudo`（密码提示）、`ssh -t`、`mysql -p`、全屏 TUI（`vim`、`top`、`nmap -iL -`）。stdout 与 stderr 共享同一条 pty 流。 |

PTY 模式在 Unix 上用 `creack/pty`，在 Windows 上用 **ConPTY**
（Win10 1809+）。daemon 直接用 `ReadFile`/`WriteFile` 读 pty master 端，
而非走 Go 的 netpoll，保证 ConPTY 输出可靠送达。

### 调整尺寸

daemon 支持 `resize` 命令，参数 `{"cols": N, "rows": N}`。
`ish attach` 进入时会自动调用一次，把 pty 尺寸同步成本地终端尺寸。

---

## Attach

`ish attach --uid <id>` 把当前终端连到一个上下文：

- 本地按键逐字节转发（Ctrl-C、方向键等发给子进程，不被本地 shell 解释）。
- 上下文输出通过长度前缀 JSON 流式协议持续推到本地终端（连续 `data`
  帧直到 `exit`/`end`）。
- 用 **Ctrl-Q** 分离（可用 `--detach-key` 自定义，支持单字面字符、
  `\xNN` 十六进制、`C-x` 表示法）。
- 本地终端置为 raw 模式，并进入**备用屏缓冲**（DECSET 1049），保护
  用户终端历史。分离时两者都恢复。
- attach 时 pty 自动 resize 到本地终端尺寸。
- 500ms 一次的退出状态轮询：子进程退出时打印提示。

建议配合 `--pty` 上下文使用：管道模式上下文不回显按键、不响应终端
控制码。

> **限制：** attach 到一个已在运行的 pty 上下文可能出现文本错位——
> `ish` 只缓冲 pty 的原始字节流，不跟踪子进程的光标状态，所以子进程
> 输出的 ANSI 转义序列假定一个光标位置，而本地终端的实际光标位置已
> 不一致。要干净的 attach 体验，请在 `ish new --pty` 后立刻 attach。
> AI agent 应优先用 `ish input` + `ish stdout --follow`，不要用
> `attach`。

---

## 架构

```
+----------------+        IPC（UDS / Named Pipe）        +------------------+
|  ish client    | <----------------------------------> |  ish daemon      |
|  （一次性）    |                                      |  （长驻）        |
+----------------+                                      |                  |
                                                       |  Context A       |
                                                       |   - cmd.Start    |
                                                       |   - RingBuffer x2|
                                                       |   - pty master   |
                                                       |  Context B       |
                                                       |   ...            |
                                                       +------------------+
```

- **Daemon**：长驻、detached、首次调用时自动拉起。持有 `Manager`
  （上下文集合），在 IPC 通道上接收长度前缀 JSON 帧。
- **Client**：薄薄一层，一次性。每次 `ish <cmd>` 调用拨号 daemon，
  写一个 `Request` 帧，读一个或多个 `Response` 帧，打印结果后退出。
- **Context**：一个子进程加两个环形缓冲（stdout、stderr），带单调
  递增 offset。客户端按 offset 读取，只拿到该 offset 之后的字节；
  `--follow` 持续流式推 `data` 帧直到 `exit`/`end`。
- **状态目录**：默认 `~/.ish`（可用 `ISH_STATE_DIR` 覆盖）。包含：
  - `daemon.addr` — IPC 地址（UDS 路径或 pipe 名）
  - `daemon.pid`  — daemon PID
  - `daemon.log`  — daemon 日志（设 `ISH_PTY_DEBUG=1` 打开 pty 调踪）

### IPC

| 平台 | 通道 | 权限 |
|------|------|------|
| Linux / macOS | `~/.ish/daemon.sock`（UDS） | `0600`，仅属主 |
| Windows | `\\.\pipe\ish-<sanitized-path>`（Named Pipe） | SDDL `D:P(A;;FA;;;OW)(A;;FA;;;BA)` — 属主 + 管理员完全控制 |

### 线协议

每条连接是"一个请求 → 一个或多个响应"。帧格式为 4 字节小端长度前缀
+ JSON 负载。

```
client -> daemon:  Request  {cmd, uid, ...}
daemon -> client:  Response {status, type, bytes, offset, ...}   （一个或多个）
```

| `cmd`     | 请求字段                                                  | 响应                                                                      |
|-----------|-----------------------------------------------------------|---------------------------------------------------------------------------|
| `health`  | —                                                         | `{status:200, data:{ok:true, pid:N}}`                                     |
| `list`    | —                                                         | `{status:200, data:[ContextInfo,...]}`                                    |
| `create`  | `name, exec, args, cwd, env, pty, cols, rows`             | `{status:200, data:ContextInfo}`                                          |
| `info`    | `uid`                                                     | `{status:200, data:ContextInfo}`                                          |
| `delete`  | `uid`                                                     | `{status:200, data:{ok:true}}`                                            |
| `input`   | `uid, data, no_newline`                                   | `{status:200, data:{ok:true, bytes:N}}`                                   |
| `kill`    | `uid`                                                     | `{status:200, data:{ok:true}}`                                            |
| `resize`  | `uid, cols, rows`                                         | `{status:200, data:{ok:true}}`                                            |
| `stream`  | `uid, stream(stdout\|stderr), offset, follow, timeout`    | `!follow` 时单帧 `{status:200, bytes, offset, exited}`；否则多个 `data` 帧后跟 `end`/`exit` |
| `wait`    | `uid, timeout`                                            | `{status:200, data:{exited:true, exitCode:N}}` 或 `{status:202, data:{exited:false}}` |
| `shutdown`| —                                                         | `{status:200, data:{ok:true}}`                                            |

`offset=-1` 表示"当前"（跳过已有数据，只流新输出）。`stream`/`wait`
中 `timeout=0` 表示"永久"。

---

## 环境变量

| 变量 | 默认值 | 用途 |
|------|--------|------|
| `ISH_STATE_DIR` | `~/.ish` | daemon 状态目录（addr/pid/log 文件 + UDS socket） |
| `ISH_PTY_DEBUG` | 未设置 | 任何非空值都会在 `daemon.log` 中开启 pty 详细日志 |

---

## 退出码

| 码 | 含义 |
|----|------|
| 0 | 成功 |
| 1 | 运行时错误（daemon 不可达、上下文不存在、子进程启动失败等） |
| 2 | 用法错误（缺少必需 flag、未知命令） |

---

## 示例

### 反弹 shell 处理

```bash
ish new --name revshell -- nc -lvvp 4444
# ... 目标连回 ...
ish input --uid revshell "id; whoami; uname -a"
ish stdout --uid revshell --offset 0 --follow --timeout 5
```

### sudo 带密码（需要 pty）

```bash
ish new --name sudo --pty -- sudo -k ls /root
ish input --uid sudo "mySecretPassword"
ish stdout --uid sudo --follow --timeout 3
```

### 交互式 ssh（需要 pty）

```bash
ish new --name ssh1 --pty -- ssh -t user@10.0.0.5
ish input --uid ssh1 "ls -la /tmp"
ish stdout --uid ssh1 --follow --timeout 3
# ... 或者让人 attach 上去 ...
ish attach --uid ssh1
# （Ctrl-Q 分离）
```

### evil-winrm 风格执行器（管道模式）

```bash
ish new --name winrm -- evil-winrm -i 10.0.0.5 -u myuser -p 'myPass'
ish input --uid winrm "whoami /priv"
ish stdout --uid winrm --follow --timeout 3
```

### 管道式二进制 payload

```bash
cat payload.bin | ish input --uid revshell --stdin --no-newline
```

### 长驻监听器多读者

```bash
# agent A 从 offset 0 开始读
ish stdout --uid revshell --offset 0 --follow --timeout 30 > out_a.txt

# 之后 agent B 只读新的（用 A 返回的 offset）
ish stdout --uid revshell --offset 12345 --follow --timeout 30 > out_b.txt
```

---

## 项目结构

```
.
├── main.go            # CLI 入口、usage/help 分派
├── client.go          # CLI 命令实现（基于帧的一次性客户端）
├── daemon.go          # Daemon：帧协议、分派器、上下文管理、自动启动
├── context.go         # Context + RingBuffer + Manager
├── attach.go          # 'ish attach' 交互模式（备用屏 + 流式）
├── flagutil.go        # flagSet 封装（可重复 --env）
├── ipc_unix.go        # UDS listen/dial/cleanup（linux/darwin/bsd）
├── ipc_windows.go     # Named Pipe listen/dial/cleanup（windows）
├── pty_unix.go        # creack/pty 集成 + waitChild
├── pty_windows.go     # ConPTY（CreatePseudoConsole + STARTUPINFOEX）
├── term_unix.go       # termios raw 模式 + 恢复 + termSize
├── term_windows.go    # console 模式 raw + 恢复 + termSize
├── proc_unix.go       # detachedSysProcAttr、killProcessTree、processExists
├── proc_windows.go    # Windows 版同上
├── go.mod
└── bin/               # 编译产物（与源码分离）
    ├── ish.exe
    ├── ish-linux-amd64
    └── ish-darwin-amd64
```

源码与编译产物分目录存放，便于发布包只打包 `bin/`。

---

## 备注

- daemon 每 5 分钟回收一次已退出超过 1 小时的上下文，避免内存无限
  增长。刚退出的上下文会保留一段时间，便于客户端读取最终输出。
- `kill` 杀整个进程树（Unix：`kill -PGID`，Windows：`taskkill /T /F`）。
- `attach` 在管道模式上下文上是尽力而为：按键会转发，但子进程不回显、
  不响应终端控制码。真正交互式会话请用 `--pty`。
- ConPTY 需要 Windows 10 1809+。更老的 Windows 上 `--pty` 路径会在
  `CreatePseudoConsole` 处失败。
