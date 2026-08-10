# 北大网盘 CLI：完整参考与故障排查

本文集中记录安装细节、登录方式、完整命令语义、诊断方法和安全边界。首次安装与日常使用
请先阅读[简明 README](../README.md)。

`pku-drive` 是面向 Linux 服务器和 SSH 终端的非官方北大网盘 CLI，通过
`https://disk.pku.edu.cn` 的 OAuth 2.0 和 AnyShare 文档 API 工作。它不安装或驱动官方
Electron 客户端，也不依赖 Python、Node.js、rclone、WebDAV 或 FUSE。

## 环境要求

- 运行环境为 Linux amd64；建议使用内核 6.6 或更新版本。
- 发布二进制无 CGO 运行时依赖。系统需要 CA 证书；打开服务器浏览器时建议安装
  `xdg-utils`。
- 构建需要 Go 1.22、GNU Make 和 GNU coreutils；`make test` 的 race detector 还需要 C
  编译工具链。
- 制作发布包另外需要 `dpkg-deb`、`jq`、GNU tar、gzip 和 `sha256sum`。
- 首次登录需要能从服务器访问 `disk.pku.edu.cn:443`，并在浏览器完成北京大学统一身份
  认证。

## 安装和升级

### Debian / Ubuntu 系统安装

从 [GitHub Releases](https://github.com/Findddx/pku-drive-cli/releases) 下载同一版本的
`.deb` 和 `SHA256SUMS`，核验后安装：

```bash
sha256sum --ignore-missing -c SHA256SUMS
sudo apt install ./pku-drive-cli_0.2.0-1_amd64.deb
/usr/bin/pku-drive version
```

系统包安装以下主要文件：

```text
/usr/bin/pku-drive
/etc/pku-drive-cli/object-pin.json
/usr/share/doc/pku-drive-cli/
```

程序和系统对象证书 pin 可供所有用户读取，但 OAuth 凭据、配置、锁和上传续传状态按用户
分别保存在各自的 XDG 目录。每个用户必须以自己的普通账户执行 `pku-drive login`。不要用
`sudo` 执行 `login`、`status`、`ls`、`mkdir`、`put`、`get`、`rm` 或 `logout`。

若 PATH 命中了以前的用户安装版：

```bash
type -a pku-drive
/usr/bin/pku-drive version
```

升级时安装新 `.deb`。卸载程序但保留系统 pin 使用 `sudo apt remove pku-drive-cli`；同时
删除系统 pin 使用 `sudo apt purge pku-drive-cli`。两者都不会删除用户 HOME 中的凭据和
状态文件。

### tar 包系统安装

没有 Debian 包管理器时，从 Releases 下载同一版本的 tar 包和校验文件：

```bash
sha256sum --ignore-missing -c SHA256SUMS
sudo tar -xzf pku-drive-cli-0.2.0-linux-amd64.tar.gz \
  --strip-components=1 -C /
/usr/local/bin/pku-drive version
```

tar 包不受包管理器跟踪。不要与 `.deb` 同时安装。

### 从源码构建

```bash
git clone https://github.com/Findddx/pku-drive-cli.git
cd pku-drive-cli
make test
make build
./bin/pku-drive version
```

`make install` 把开发构建原子安装到当前用户的 `~/.local/bin/pku-drive`；不要执行
`sudo make install`。如果 PATH 不含该目录，可在 shell 配置中加入：

```bash
export PATH="$HOME/.local/bin:$PATH"
```

`make package` 还需要一个已经通过独立可信渠道核验的对象证书 pin。默认读取
`${XDG_CONFIG_HOME:-$HOME/.config}/pku-drive-cli/object-pin.json`，也可指定规范化绝对路径：

```bash
PACKAGE_PIN_FILE=/absolute/path/object-pin.json make package
```

pin 文件必须归当前用户所有、权限 `0600`、只有一个硬链接，其父目录必须归当前用户所有且
权限 `0700`。打包要求 Git 工作树干净，并在 `dist/` 生成 `.deb`、tar 包和
`SHA256SUMS`。构建和打包均使用普通用户。

## 登录与认证

### 浏览器运行在服务器、VNC 或 X11 中

```bash
pku-drive login
pku-drive status --json
```

当 `DISPLAY` 或 `WAYLAND_DISPLAY` 非空时，工具尝试调用 `xdg-open`。没有自动出现窗口时，
手动在服务器浏览器中打开终端打印的授权 URL；命令仍会继续等待回调。

`login` 若发现 access token 仍新鲜，或 refresh token 刷新成功，会直接返回而不打开
浏览器。这是正常行为，可用 `status` 验证。

### 纯 SSH：浏览器运行在本地电脑

推荐使用回调粘贴模式，不需要 SSH 端口转发：

1. 在服务器 SSH 会话中运行并保持会话等待：

   ```bash
   env -u DISPLAY -u WAYLAND_DISPLAY pku-drive login --paste-callback
   ```

2. 在本地浏览器打开终端打印的完整授权 URL，完成统一身份认证。
3. 浏览器跳到 `http://127.0.0.1:PORT/callback?code=...&state=...` 后，页面显示无法连接是
   正常的；这里的 `127.0.0.1` 指本地电脑。
4. 从地址栏复制**完整回调 URL**，粘贴到仍在等待的 SSH 终端并回车。不要只复制
   `code`，不要修改或解码 URL。
5. 看到 `Login successful.` 后运行 `pku-drive status --json`。

粘贴输入不回显。回调 URL 含一次性 code 和 `state`，不要写入 shell 历史、日志、工单或
聊天。非交互式 SSH 命令需要加 `ssh -t` 分配 PTY。异常中断后终端若不再回显，可执行
`stty echo` 或 `reset`。

### Windows PowerShell + OpenSSH

PowerShell 窗口中先 SSH 到服务器，再运行粘贴模式：

```powershell
ssh.exe USER@SERVER
# 以下命令在服务器 shell 内运行：
env -u DISPLAY -u WAYLAND_DISPLAY pku-drive login --paste-callback
```

在本地打开授权 URL 时使用单引号，避免 URL 中的 `&` 被 PowerShell 解释：

```powershell
Start-Process '完整授权URL'
```

认证完成后从浏览器地址栏复制完整回调 URL，粘贴回 SSH 窗口。不要把回调 URL 当作
PowerShell 命令执行。

### 可选：SSH 端口转发自动回调

普通 `pku-drive login` 会在授权 URL 的 `redirect_uri` 中显示服务器回调端口。必须在打开
授权 URL 前，从本地另开终端建立相同端口的转发：

```bash
PORT=授权URL中显示的端口
ssh -o ExitOnForwardFailure=yes -N \
  -L "127.0.0.1:${PORT}:127.0.0.1:${PORT}" USER@SERVER
```

两端都必须使用 `127.0.0.1` 和同一端口。若 sshd 禁止 TCP forwarding，请改用
`--paste-callback`，不要把回调监听暴露到 `0.0.0.0`。

### 登录时限、重新认证和切换账号

`login` 的五分钟时限从命令启动开始计算，包括凭据锁等待、刷新、浏览器授权、回调和 code
换 token。超时、`Ctrl-C`、SSH 断开后，旧 URL、旧 `state` 和旧端口都不可复用，应重新
运行登录命令并只使用最新 URL。

普通网盘命令只自动刷新 token，不会自行打开浏览器。遇到 `login required` 时运行：

```bash
pku-drive login --paste-callback
pku-drive status
```

切换账号：

```bash
pku-drive logout
pku-drive login --paste-callback
```

普通注销若无法在服务端撤销 token，会保留本地凭据并报错。只有明确接受 token 可能仍在
服务端有效时，才使用：

```bash
pku-drive logout --local-only
```

## 完整命令参考

```text
pku-drive login [--paste-callback] [--json]
pku-drive status [--json]
pku-drive ls [REMOTE_PATH] [--json]
pku-drive mkdir REMOTE_PATH [--parents] [--json]
pku-drive put LOCAL_FILE REMOTE_FILE_OR_DIR [--overwrite] [--quiet] [--json]
pku-drive get REMOTE_FILE LOCAL_FILE_OR_EXISTING_DIR [--overwrite] [--quiet] [--json]
pku-drive rm REMOTE_PATH [--recursive] [--json]
pku-drive logout [--local-only] [--json]
pku-drive version [--json]
```

布尔选项可写成 `--flag`、`--flag=true` 或 `--flag=false`，也可放在普通参数前后。同一选项
重复会报错；用 `--` 结束选项解析。

远端路径必须是以 `/` 开头的绝对名字路径。重复斜线和 `.` 会规范化，`..` 会被拒绝。
含空格或中文的路径建议用单引号包围。

### 列目录和创建目录

```bash
pku-drive ls /
pku-drive ls '/个人文档/项目'
pku-drive mkdir '/个人文档/项目/数据'
pku-drive mkdir '/个人文档/项目/数据/2026' --parents
```

省略 `ls` 的路径等同于 `/`。`mkdir` 不加 `--parents` 时父目录必须存在；已有同名目录视为
成功，同名文件是冲突。

### 上传

```bash
pku-drive put ./report.csv '/个人文档/项目/report.csv'
pku-drive put ./report.csv '/个人文档/项目/'
pku-drive put ./report.csv '/个人文档/项目/report.csv' --overwrite
pku-drive put ./large.bin '/个人文档/项目/large.bin' --quiet
```

参数顺序始终是本地文件在前、网盘路径在后。远端目标以 `/` 结尾或解析为已有目录时，保留
本地文件名。父目录必须存在。默认绝不覆盖同名文件；`--overwrite` 才创建已有文件的新
版本，不会自动改名。

本地源必须是普通文件，目录和符号链接会被拒绝。工具根据服务端限制选择秒传、单次 PUT
或 multipart。分片上传中断后，重复完全相同的命令即可校验并续传缺失分片；覆盖场景仍需
带 `--overwrite`。本地文件发生变化、状态损坏或远端目标身份不一致时不会冒险复用状态。

### 下载

```bash
pku-drive get '/个人文档/项目/report.csv' ./report.csv
pku-drive get '/个人文档/项目/report.csv' ./downloads/
pku-drive get '/个人文档/项目/report.csv' ./report.csv --overwrite
pku-drive get '/个人文档/项目/large.bin' ./large.bin --quiet
```

参数顺序始终是网盘文件在前、本地目标在后。目标是已有目录时，使用网盘文件名；目标以
`/` 结尾时，该目录必须已经存在。远端目录不能下载。

下载先在目标目录创建私有临时文件，校验响应长度和服务端可用的 MD5 后再原子发布。默认
拒绝覆盖已有本地文件；`--overwrite` 仅覆盖安全的普通文件，不跟随符号链接。当前下载不
支持断点续传；中断后临时文件会清理，重新执行命令即可。

### 删除

```bash
pku-drive rm '/个人文档/项目/report.csv'
pku-drive rm '/个人文档/项目/旧数据' --recursive
```

文件不需要 `--recursive`。任何目录都必须显式加 `--recursive`，因为服务端目录删除接口会
处理整棵目录树。删除通常将对象移入网盘回收站；若服务器启用删除审核，成功输出为已提交
审核。虚拟根 `/` 和顶级文档库不能删除。

网络在删除请求提交后中断时，结果可能不明确。工具会直接返回错误，不会盲目重放非幂等
删除；先重新 `ls` 确认当前状态，再决定下一步。

### 进度输出

上传和下载的进度写到 stderr，最终结果写到 stdout。`--quiet` 只抑制周期进度，不抑制最终
结果或错误。stderr 不是交互式终端时默认不输出周期进度。

## JSON 和退出码

支持 `--json` 的命令在 stdout 只输出一个最终 JSON 对象；诊断和登录授权链接仍写到
stderr。脚本不要把 stderr 合并进 JSON 管道，并应同时检查退出码和 `ok`。

成功对象共同包含 `ok:true` 和 `operation`。下面只展示字段形状，值不是实际账户数据：

```json
{"ok":true,"operation":"get","remote_path":"/个人文档/项目/report.csv","remote_id":"example-id","revision":"1","local_path":"/tmp/report.csv","size":123}
{"ok":true,"operation":"rm","remote_path":"/个人文档/项目/report.csv","remote_id":"example-id","type":"file","status":"deleted","pending_review":false}
```

失败对象形如：

```json
{"ok":false,"category":"auth","message":"login required"}
```

| 代码 | JSON `category` | 含义 |
|---:|---|---|
| 0 | — | 成功 |
| 2 | `usage` | 命令、参数、选项或传输计划错误 |
| 3 | `auth` | 未登录、刷新失败或认证失败 |
| 4 | `remote` | 远端不存在、无权限、名称冲突或不允许删除 |
| 5 | `network` | 网络、限流或服务端错误 |
| 6 | `local` | 本地文件、输出或安全存储失败 |
| 7 | `integrity` | 续传状态或传输完整性核验失败 |
| 130 | `interrupted` | `SIGINT`、`SIGTERM`、命令超时或取消 |

`login` 时限为五分钟；`status`、`ls`、`mkdir`、`rm`、`logout` 为两分钟；`put` 和 `get`
为 24 小时。

## 常见故障

| 现象 | 处理 |
|---|---|
| `login` 立即成功但没有浏览器 | 现有 token 仍有效或刷新成功；运行 `pku-drive status --json`。 |
| 已打印授权 URL，但服务器没有浏览器窗口 | 手动在服务器浏览器打开；若浏览器在 SSH 客户端，取消后改用 `login --paste-callback`。 |
| 浏览器跳到 `127.0.0.1` 后显示无法连接 | 粘贴模式下复制地址栏完整 URL 并粘贴回终端；普通模式应重新登录并预先建立 SSH 转发。 |
| `state` 校验失败 | 使用了旧 URL、修改了 URL 或混用了并发登录；终止其他登录并重新开始。 |
| `administratively prohibited` | sshd 禁止端口转发；改用 `--paste-callback`。 |
| `login required` | 运行 `pku-drive login` 或 `pku-drive login --paste-callback`。 |
| `object-store trust configuration rejected` | 对象证书 pin 缺失、损坏、权限不安全或证书已轮换；通过可信渠道核验新 SPKI 后更新，不要关闭 TLS 校验。 |
| `remote path ... not found` | 先运行 `pku-drive ls /`，逐级确认顶级文档库和目录名称。 |
| 上传提示目标已存在 | 换远端文件名，或确认需要覆盖后加 `--overwrite`。 |
| 下载提示本地目标已存在 | 换本地目标，或确认需要覆盖后加 `--overwrite`。 |
| 删除目录提示需要 recursive | 确认整棵目录树都应移入回收站，再加 `--recursive`。 |
| 找不到 `pku-drive` | 运行 `type -a pku-drive`；系统包使用 `/usr/bin/pku-drive`，tar 包使用 `/usr/local/bin/pku-drive`。 |
| 命令长时间无进度 | 去掉 `--quiet` 并在交互终端重试；检查服务器 DNS、代理、隧道和到 `disk.pku.edu.cn:443` 的连通性。 |

OAuth 和控制面请求使用 Go 标准代理变量 `HTTP_PROXY`、`HTTPS_PROXY`、`NO_PROXY` 及其
小写形式；代理变量必须在启动命令前设置。PKU 对象存储在启用 SPKI pin 时使用不经过代理
的隔离传输；标准 CA 模式沿用 Go 的代理环境。浏览器访问本地 loopback 回调不经过 CLI 的
代理设置。

## 本地文件与安全边界

| 内容 | 默认位置 | 权限 |
|---|---|---:|
| 配置与 OAuth 凭据 | `~/.config/pku-drive-cli/` | 目录 `0700`、文件 `0600` |
| 用户对象证书 pin | `~/.config/pku-drive-cli/object-pin.json` | `0600` |
| 系统对象证书 pin | `/etc/pku-drive-cli/object-pin.json` | `root:root 0644` |
| 上传续传状态 | `~/.local/state/pku-drive-cli/uploads/` | 目录 `0700`、文件 `0600` |

工具拒绝配置和状态路径中的符号链接、错误所有者、异常类型或过宽权限，不会静默修复已有
对象。`credentials.json` 含 access token、refresh token 和动态客户端 secret，不要提交、
同步或复制。续传状态不保存 token、Authorization header 或完整签名对象 URL。

控制面始终使用系统 CA 和主机名校验，不提供 `--insecure`。对象数据面可以使用经过核验的
SPKI pin，仍校验证书主机名和有效期。OAuth Bearer token 不会发送到对象存储主机，签名
请求的重定向会被拒绝。

## 明确不支持

- Windows 或 macOS 原生二进制；这些系统可以作为 SSH 客户端和浏览器使用。
- 下载目录、目录同步、WebDAV、FUSE、后台 daemon 或自动更新。
- 下载断点续传；上传支持受校验的 multipart 续传。
- 移动、复制、重命名或操作回收站内容。
- 读取或复用 Chrome Cookie、Local Storage、登录数据库或 remote debugging。
- 把北大网盘当作用户可配置的 S3，或宽泛关闭 TLS 校验。

协议依据和兼容处理见 [API 与兼容性说明](api-notes.md)。
