# 故障排查与高级说明

日常安装和命令示例见[项目 README](../README.md)。本文用于定位认证、网络、分享链接和传输
问题，并记录脚本退出码与本地状态位置。

## 运行与构建环境

- 发布包支持 Linux amd64，运行时需要系统 CA 证书；自动打开服务器浏览器时建议安装
  `xdg-utils`。
- 源码构建需要 Go 1.22、GNU Make 和 GNU coreutils；race detector 需要 C 编译工具链。
- 制作发布包还需要 `dpkg-deb`、`jq`、GNU tar、gzip 和 `sha256sum`。
- 登录、列目录和传输需要服务器可访问 `disk.pku.edu.cn:443` 及响应中返回的对象存储主机。

## 安装与升级

### 命令未找到或版本不一致

检查 shell 实际使用的程序：

```bash
type -a pku-drive
/usr/bin/pku-drive version
/usr/local/bin/pku-drive version
```

`.deb` 默认安装到 `/usr/bin/pku-drive`，tar 包默认安装到
`/usr/local/bin/pku-drive`。避免同时安装两种系统包。升级时安装新版包并重新执行
`pku-drive version`；升级不会删除用户凭据。

每位系统用户都要用自己的普通账号执行 `pku-drive login`。使用 `sudo` 登录会把凭据保存到
root 的配置目录，普通用户随后仍会显示未登录。

### 源码安装

`make install` 原子安装到当前用户的 `~/.local/bin/pku-drive`。若 shell 找不到它：

```bash
export PATH="$HOME/.local/bin:$PATH"
```

打包需要已通过可信渠道核验的对象证书 pin。默认读取
`${XDG_CONFIG_HOME:-$HOME/.config}/pku-drive-cli/object-pin.json`，也可以指定绝对路径：

```bash
PACKAGE_PIN_FILE=/absolute/path/object-pin.json make package
```

pin 文件须归当前用户所有、权限为 `0600`，父目录权限为 `0700`。打包命令要求 Git 工作树
干净。

## 登录与回调

### SSH 终端没有收到浏览器回调

浏览器运行在本地电脑时使用粘贴模式：

```bash
env -u DISPLAY -u WAYLAND_DISPLAY pku-drive login --paste-callback
```

在本地浏览器完成认证后，复制地址栏的完整
`http://127.0.0.1:PORT/callback?code=...&state=...` URL，粘贴到等待中的 SSH 终端。输入会隐藏。
只复制 `code`、使用旧回调或修改 URL 都会导致校验失败。重新执行登录后只使用最新链接。

PowerShell 可用以下方式打开终端给出的授权链接：

```powershell
Start-Process '完整授权URL'
```

单引号可避免 `&` 被 PowerShell 解析。回调 URL 应粘贴到 SSH 会话内的输入提示，不要在
PowerShell 中执行。通过非交互 SSH 启动时使用 `ssh -t` 分配终端。

异常退出后若终端停止回显，运行：

```bash
stty echo
reset
```

### SSH 端口转发

普通 `pku-drive login` 也可借助本地端口转发接收自动回调。先读取授权 URL 中的端口，再从
本地另开终端：

```bash
PORT=授权URL中的端口
ssh -o ExitOnForwardFailure=yes -N \
  -L "127.0.0.1:${PORT}:127.0.0.1:${PORT}" USER@SERVER
```

服务器提示 `administratively prohibited` 表示 sshd 禁止转发，改用 `--paste-callback`。
回调监听应保持在 `127.0.0.1`。

### 登录状态异常

| 现象 | 处理 |
|---|---|
| `login` 立即成功，没有浏览器 | 当前 token 仍有效或已成功刷新；运行 `pku-drive status --json`。 |
| `login required` | 运行 `pku-drive login`；纯 SSH 使用 `--paste-callback`。 |
| `state` 校验失败 | 关闭并发登录流程，重新执行登录并使用新 URL。 |
| 登录超过五分钟 | 重新执行登录；授权 URL、回调端口和 `state` 均为一次性。 |
| 需要切换账号 | 先 `pku-drive logout`，再重新登录。 |

普通注销会尝试在服务端撤销 token。网络故障导致撤销失败时，本地凭据会保留；确认接受远端
token 可能继续有效后，才使用 `pku-drive logout --local-only`。

## 分享链接

| 现象 | 处理 |
|---|---|
| 公开分享提示需要登录 | 链接可能被改为组织范围，或分享策略已更新；登录后重试。 |
| 组织范围分享无权限 | 确认当前登录账号属于分享指定范围，并让分享者检查权限。 |
| 链接要求提取码 | 当前版本不处理提取码；请让分享者提供无需提取码的链接或通过网页下载。 |
| 链接要求手机验证 | 当前版本不处理手机验证；请使用浏览器完成下载。 |
| 交互界面无法启动 | 需要 TTY；脚本中用 `ls --share ... --json` 和带明确相对路径的 `get --share ... --json`。 |
| 分享内文件找不到 | 先列出分享根目录，再逐层对照大小写和名称；分享路径不以 `/` 开头。 |
| 分享已失效 | 请分享者检查到期时间、删除状态和访问范围后重新生成链接。 |
| 批量下载中途失败 | 已完成文件会保留，错误会显示完成数量；重新选择未完成文件，或确认覆盖后加入 `--overwrite`。 |

分享 URL、临时 link token、Cookie 和下载签名可能授予文件访问权限。不要把它们写入日志、
Issue、聊天记录或公开的命令历史。建议将链接放入受控的脚本变量，使用完后清除历史。

## 网络与代理

控制面使用 Go 标准代理环境变量 `HTTP_PROXY`、`HTTPS_PROXY`、`NO_PROXY` 及其小写形式。
变量应在启动命令前设置。对象存储在启用 SPKI pin 时使用隔离连接；标准 CA 模式遵循代理
环境。浏览器访问本地 loopback 回调不经过 CLI 代理。

基础连通性检查：

```bash
getent hosts disk.pku.edu.cn
curl -I --connect-timeout 10 https://disk.pku.edu.cn/
```

校园 VLAN、代理或 tunnel 环境出现长时间无响应时，检查 DNS、MTU、代理规则和到
`disk.pku.edu.cn:443` 的连接。`--quiet` 会隐藏周期进度，排查时先移除该选项。

`object-store trust configuration rejected` 表示对象证书 pin 缺失、损坏、权限不安全或证书
已经轮换。通过可信渠道核验新的 SPKI 后更新配置；工具没有跳过 TLS 校验的选项。

## 文件传输与删除

| 现象 | 处理 |
|---|---|
| 网盘路径不存在 | 从 `pku-drive ls /` 开始逐级检查文档库与目录名称。 |
| 上传目标已存在 | 换文件名，或确认覆盖后加入 `--overwrite`。 |
| 本地下载目标已存在 | 换本地路径，或确认覆盖后加入 `--overwrite`。 |
| 上传中断 | 对未变化的本地文件和同一远端目标重复原命令，工具会核验并续传已有分片。 |
| 下载中断 | 重新运行命令；未完成的临时文件会清理。 |
| 删除目录要求 recursive | 确认目录树后加入 `--recursive`。 |
| 删除时网络中断 | 先重新列目录确认结果，再决定是否重试。 |

上传源必须是普通文件。下载会在目标目录写入私有临时文件，完成长度与可用 MD5 校验后原子
发布；`--overwrite` 仅覆盖安全的普通文件，不跟随符号链接。上传和下载的进度写到 stderr，
最终结果写到 stdout。

## JSON 与退出码

`--json` 模式在 stdout 输出一个最终 JSON 对象，诊断与授权链接写到 stderr。脚本应同时
检查退出码和 `ok`。典型失败结果：

```json
{"ok":false,"category":"auth","message":"login required"}
```

| 退出码 | JSON `category` | 含义 |
|---:|---|---|
| 0 | — | 成功 |
| 2 | `usage` | 参数、选项或传输计划错误 |
| 3 | `auth` | 未登录、刷新失败或认证失败 |
| 4 | `remote` | 远端不存在、无权限、冲突或分享失效 |
| 5 | `network` | 网络、限流或服务端错误 |
| 6 | `local` | 本地文件、输出或安全存储失败 |
| 7 | `integrity` | 续传状态或传输完整性失败 |
| 130 | `interrupted` | 信号、超时或用户取消 |

登录时限为五分钟；元数据命令通常为两分钟；上传和下载最长运行 24 小时。

## 本地状态与安全边界

| 内容 | 默认位置 | 权限 |
|---|---|---:|
| OAuth 配置与凭据 | `~/.config/pku-drive-cli/` | 目录 `0700`、文件 `0600` |
| 用户对象证书 pin | `~/.config/pku-drive-cli/object-pin.json` | `0600` |
| 系统对象证书 pin | `/etc/pku-drive-cli/object-pin.json` | `root:root 0644` |
| 上传续传状态 | `~/.local/state/pku-drive-cli/uploads/` | 目录 `0700`、文件 `0600` |

`credentials.json` 包含 access token、refresh token 和动态客户端 secret，不应复制、同步或
提交。共享链接会话只在命令进程内存中保存，不写入凭据目录。续传状态不保存 token、
Authorization header 或完整签名 URL。

控制面始终验证系统 CA 与主机名。OAuth Bearer token 只发往北大网盘控制面；对象存储使用
服务端签发的短期下载或上传授权。

## 功能范围

当前发布版面向 Linux amd64 单机命令行工作流，支持单文件上传、单文件下载、分享文件批量
选择、上传分片续传和网盘目录管理。目录同步、FUSE/WebDAV 挂载、后台 daemon、回收站管理、
移动/复制/重命名和下载断点续传尚未纳入命令集。

协议端点和兼容处理见 [API 与兼容性说明](api-notes.md)。
