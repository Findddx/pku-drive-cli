# 北大网盘轻量 CLI

`pku-drive` 是面向 Linux 服务器和 SSH 终端的北大网盘命令行工具。它支持浏览器登录、
目录浏览、文件上传与下载、创建目录、删除，以及直接下载他人分享的文件。发布包是 Linux
amd64 静态单文件；系统中的每位用户使用自己的北大账号和凭据。

## 安装

### Debian / Ubuntu

```bash
curl -LO https://github.com/Findddx/pku-drive-cli/releases/download/v0.3.0/pku-drive-cli_0.3.0-1_amd64.deb
curl -LO https://github.com/Findddx/pku-drive-cli/releases/download/v0.3.0/SHA256SUMS
grep '  pku-drive-cli_0.3.0-1_amd64.deb$' SHA256SUMS | sha256sum -c -
sudo apt install ./pku-drive-cli_0.3.0-1_amd64.deb
pku-drive version
```

### 其他 Linux amd64 发行版

```bash
curl -LO https://github.com/Findddx/pku-drive-cli/releases/download/v0.3.0/pku-drive-cli-0.3.0-linux-amd64.tar.gz
curl -LO https://github.com/Findddx/pku-drive-cli/releases/download/v0.3.0/SHA256SUMS
grep '  pku-drive-cli-0.3.0-linux-amd64.tar.gz$' SHA256SUMS | sha256sum -c -
sudo tar -xzf pku-drive-cli-0.3.0-linux-amd64.tar.gz --strip-components=1 -C /
/usr/local/bin/pku-drive version
```

同一台机器选择一种系统安装方式即可。安装后所有用户都能运行 `pku-drive`，每位用户以
自己的普通 Linux 账号登录；日常命令无需 `sudo`。

### 从源码构建

```bash
git clone https://github.com/Findddx/pku-drive-cli.git
cd pku-drive-cli
make test
make build
./bin/pku-drive version
```

## 登录

服务器有桌面、VNC 或 X11 浏览器时：

```bash
pku-drive login
pku-drive status
```

通过 SSH 登录服务器、浏览器运行在本地电脑时：

```bash
pku-drive login --paste-callback
```

在本地浏览器打开终端显示的授权链接并完成统一身份认证。浏览器最后跳转到
`http://127.0.0.1:PORT/callback?...` 后，复制地址栏中的**完整 URL**，粘贴回 SSH 终端并
回车。页面显示无法连接不影响粘贴认证。回调 URL 含一次性认证信息，请勿分享、记录或写入
命令历史。

## 路径规则

- 网盘路径是以 `/` 开头的绝对路径，例如 `'/个人文档/课题/data.csv'`。
- 本地路径与普通 shell 命令相同，可以是相对路径或绝对路径。
- 路径含空格、中文或 shell 特殊字符时，用单引号包围。
- 分享链接内的路径相对于分享根目录，不以 `/` 开头，例如 `'数据/样本.csv'`。

## 命令速查

| 操作 | 命令 |
|---|---|
| 登录 | `pku-drive login [--paste-callback]` |
| 查看登录状态 | `pku-drive status` |
| 列出网盘目录 | `pku-drive ls [网盘目录]` |
| 创建目录 | `pku-drive mkdir '网盘目录' [--parents]` |
| 上传文件 | `pku-drive put 本地文件 '网盘文件或目录'` |
| 下载文件 | `pku-drive get '网盘文件' 本地文件或目录` |
| 上传目录 | `pku-drive put --recursive 本地目录 '网盘目录' [--jobs=N]` |
| 下载目录 | `pku-drive get --recursive '网盘目录' 本地目录 [--jobs=N]` |
| 浏览分享链接 | `pku-drive ls --share 分享链接 [分享内目录]` |
| 下载分享文件 | `pku-drive get --share 分享链接 本地目录 [分享内文件...]` |
| 删除 | `pku-drive rm '网盘文件或目录' [--recursive]` |
| 注销 | `pku-drive logout` |
| 查看版本 | `pku-drive version` |

## 常用操作

### 查看目录

```bash
pku-drive ls /
pku-drive ls '/个人文档/课题'
```

### 上传文件

参数顺序是 `put 本地文件 '网盘文件或目录'`：

```bash
# 上传并指定网盘文件名
pku-drive put ./report.csv '/个人文档/课题/report.csv'

# 上传到已有网盘目录，保留文件名 report.csv
pku-drive put ./report.csv '/个人文档/课题/'

# 覆盖网盘中的同名文件
pku-drive put ./report.csv '/个人文档/课题/report.csv' --overwrite

# 大文件上传；中断后重复同一命令可继续已校验的分片
pku-drive put ./dataset.tar '/个人文档/课题/dataset.tar' --quiet
```

### 下载自己的网盘文件

参数顺序是 `get '网盘文件' 本地文件或目录`：

```bash
# 下载并指定本地文件名
pku-drive get '/个人文档/课题/report.csv' ./report.csv

# 下载到已有目录，保留文件名 report.csv
pku-drive get '/个人文档/课题/report.csv' ./downloads/

# 覆盖本地同名文件
pku-drive get '/个人文档/课题/report.csv' ./report.csv --overwrite
```

### 创建与删除目录

```bash
# 父目录已经存在
pku-drive mkdir '/个人文档/课题/数据'

# 同时创建缺少的父目录
pku-drive mkdir '/个人文档/课题/数据/2026' --parents

# 删除文件，通常移入网盘回收站
pku-drive rm '/个人文档/课题/report.csv'

# 删除目录及其中内容
pku-drive rm '/个人文档/课题/旧数据' --recursive
```

### 递归下载/上传目录

`get --recursive` 与 `put --recursive` 按整棵目录树传输。布局是 **rsync 式**：源目录的直接
子项落在目标的顶层，不创建以源目录名命名的子目录：

```bash
mkdir -p ./dst
pku-drive get --recursive '/个人文档/课题' ./dst
# 产生 ./dst/report.csv、./dst/数据/a.csv（而不是 ./dst/课题/...）

pku-drive put --recursive ./backup '/个人文档/备份'
# 网盘上产生 /个人文档/backup 的每个文件（而不是 /个人文档/备份/backup/...）
```

两方向都可以加 `--jobs=N`（1..64，默认 1 即严格串行）并发传输单个文件：

```bash
pku-drive get --recursive '/个人文档/课题' ./dst --jobs=4
pku-drive put --recursive ./backup '/个人文档/备份' --jobs=4 --json
```

`--jobs` 是带值 flag，遵循 Go `flag` 包的标准行为：`--jobs=4` 在任何位置都可靠；
`--jobs 4`（空格形式）只有当值紧跟在 flag 后面时才有效——若 flag 写在位置参数之后
（例如 `get --recursive '/a/b' ./dst --jobs 4`），重排逻辑会把紧跟的 `'/a/b'` 当作
`--jobs` 的值导致解析失败退出 2。建议统一使用 `--jobs=4` 等号写法。

不对称的目标目录约定（与单文件命令一致）：

- **get 的本地目录必须已存在**：工具全程以不跟随符号链接的安全方式绑定目标目录，
  不会替你创建本地路径（`O_NOFOLLOW` 安全姿态）。
- **put 的远端目录不存在时自动逐级创建**：复用 `mkdir --parents` 的创建与冲突和解
  纪律，绝不重复发送非幂等创建请求。

已存在同名文件默认**跳过**并计入 `skipped`（原因 `exists`），加 `--overwrite` 才覆盖。
这与单文件 get/put 无 `--overwrite` 时直接报错不同：跳过语义让中断后**重跑同一命令即可
续传**——已完成的文件自动跳过，只有剩余文件继续传输。上传侧的本地符号链接同样跳过
（原因 `symlink`，不跟随）；本地空目录不会在网盘创建。整库路径（如 `'/个人文档'`）
允许递归；虚拟根 `'/'` 拒绝（用法错误）。

部分失败契约：批内在**第一个文件错误**处停止，退出码等于该错误的类别（remote=4 /
network=5 / local=6 / integrity=7 / interrupted=130）。错误消息包含已完成数量，
`--json` 失败体额外携带已完成（`downloads`/`uploads`）与跳过（`skipped`）列表，
脚本可据此增量重跑：

```jsonc
{"ok":false,"category":"network","message":"download stopped after 1 completed file(s); ...",
 "downloads":[...],"skipped":[]}
```

## 下载分享链接

公开分享（anonymous）可直接浏览和下载。组织范围分享（realname）先运行 `pku-drive login`
或 `pku-drive login --paste-callback`，工具会使用当前用户身份检查访问权限。

交互式选择：

```bash
mkdir -p ./downloads
pku-drive get --share 'https://disk.pku.edu.cn/link/...' ./downloads/
```

本地目标必须是已存在的目录。单文件分享会直接下载；文件夹分享每次显示当前目录的一层
内容，进入目录时再加载下一层，因此大型分享也能快速浏览。选择会在切换目录后保留，下载
时维持分享中的相对目录结构。批量下载按文件逐个校验和落盘；若中途失败，终端会报告已完成
数量，已完成文件保留在目标目录。重试时可只选择未完成文件，或确认覆盖后加入 `--overwrite`。

| 按键 | 操作 |
|---|---|
| `↑` / `↓` 或 `k` / `j` | 移动光标 |
| `→` 或 `l` | 进入目录 |
| `←`、`h` 或 `Backspace` | 返回上级目录 |
| `Space` | 选中或取消当前文件 |
| `Enter` | 确认并开始下载 |
| `q` 或 `Esc` | 取消 |

脚本和无交互终端先用 JSON 列目录，再按相对路径下载一个或多个文件：

```bash
pku-drive ls --share 'https://disk.pku.edu.cn/link/...' --json
pku-drive ls --share 'https://disk.pku.edu.cn/link/...' '数据' --json
pku-drive get --share 'https://disk.pku.edu.cn/link/...' ./downloads/ \
  'README.pdf' '数据/样本.csv' --json
```

当前版本处理无需提取码和手机验证的分享链接。相关错误处理见
[故障排查](docs/troubleshooting.md#分享链接)。

## JSON 输出

在命令末尾加入 `--json` 可获得结构化结果：

```bash
pku-drive status --json
pku-drive ls '/个人文档/课题' --json
pku-drive put ./report.csv '/个人文档/课题/report.csv' --json
```

脚本应同时检查进程退出码和 JSON 中的 `ok` 字段。诊断信息与登录授权链接输出到 stderr。

## 文档

- [故障排查与高级说明](docs/troubleshooting.md)
- [API 与兼容性说明](docs/api-notes.md)
- [安全策略](SECURITY.md)
- [参与贡献](CONTRIBUTING.md)

本项目是社区维护的非官方工具，与北京大学及爱数无隶属关系。
