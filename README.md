# 北大网盘轻量 CLI

`pku-drive` 是一个面向 Linux 服务器和 SSH 终端的非官方北大网盘命令行工具，支持浏览器
登录、列目录、创建目录、上传、下载、删除和注销。发布包是 Linux amd64 静态单文件；每个
服务器用户使用各自的北大账号和凭据。

## 安装

Debian / Ubuntu 推荐安装 `.deb`：

```bash
curl -LO https://github.com/Findddx/pku-drive-cli/releases/download/v0.2.0/pku-drive-cli_0.2.0-1_amd64.deb
curl -LO https://github.com/Findddx/pku-drive-cli/releases/download/v0.2.0/SHA256SUMS
grep '  pku-drive-cli_0.2.0-1_amd64.deb$' SHA256SUMS | sha256sum -c -
sudo apt install ./pku-drive-cli_0.2.0-1_amd64.deb
pku-drive version
```

其他 Linux amd64 发行版可安装 tar 包：

```bash
curl -LO https://github.com/Findddx/pku-drive-cli/releases/download/v0.2.0/pku-drive-cli-0.2.0-linux-amd64.tar.gz
curl -LO https://github.com/Findddx/pku-drive-cli/releases/download/v0.2.0/SHA256SUMS
grep '  pku-drive-cli-0.2.0-linux-amd64.tar.gz$' SHA256SUMS | sha256sum -c -
sudo tar -xzf pku-drive-cli-0.2.0-linux-amd64.tar.gz --strip-components=1 -C /
/usr/local/bin/pku-drive version
```

不要同时安装两种包。系统安装后所有 Linux 用户都能运行程序，但每个用户必须以自己的
普通账号分别登录；日常网盘命令不要加 `sudo`。

从源码构建：

```bash
git clone https://github.com/Findddx/pku-drive-cli.git
cd pku-drive-cli
make test
make build
./bin/pku-drive version
```

## 登录

浏览器运行在服务器、VNC 或 X11 环境时：

```bash
pku-drive login
pku-drive status
```

浏览器运行在 SSH 客户端电脑上时：

```bash
pku-drive login --paste-callback
```

打开终端打印的授权链接并完成统一身份认证。浏览器最后跳到
`http://127.0.0.1:PORT/callback?...` 时，即使页面显示无法连接，也复制地址栏中的**完整
URL**，粘贴到 SSH 终端的隐藏输入提示并回车。该 URL 含一次性认证信息，不要分享或保存。

## 常用操作

远端路径使用以 `/` 开头的绝对路径；含空格或中文时用单引号包围。

### 查看状态和目录

```bash
pku-drive status
pku-drive ls /
pku-drive ls '/个人文档/项目'
```

### 创建文件夹

```bash
# 父目录必须存在
pku-drive mkdir '/个人文档/项目/数据'

# 同时创建缺少的各级父目录
pku-drive mkdir '/个人文档/项目/数据/2026' --parents
```

### 上传

参数顺序是：`put 本地文件 '网盘文件或目录'`。

```bash
# 指定网盘文件名
pku-drive put ./report.csv '/个人文档/项目/report.csv'

# 上传到已有网盘目录，保留本地文件名 report.csv
pku-drive put ./report.csv '/个人文档/项目/'

# 覆盖已有同名文件
pku-drive put ./report.csv '/个人文档/项目/report.csv' --overwrite

# 不显示传输进度；上传中断后重复同一条命令即可续传
pku-drive put ./large.bin '/个人文档/项目/large.bin' --quiet
```

默认不覆盖已有文件；只有显式使用 `--overwrite` 才会覆盖。

### 下载

参数顺序是：`get '网盘文件' 本地文件或已有目录`。

```bash
# 指定本地文件名
pku-drive get '/个人文档/项目/report.csv' ./report.csv

# 下载到已有本地目录，保留网盘文件名 report.csv
pku-drive get '/个人文档/项目/report.csv' ./downloads/

# 覆盖已有本地文件
pku-drive get '/个人文档/项目/report.csv' ./report.csv --overwrite

# 不显示传输进度
pku-drive get '/个人文档/项目/large.bin' ./large.bin --quiet
```

下载先写入同目录临时文件，完整性核验成功后再原子放到目标位置；默认不覆盖本地文件。

### 删除

```bash
# 删除文件：移入网盘回收站
pku-drive rm '/个人文档/项目/report.csv'

# 删除目录及其全部内容：必须显式加 --recursive
pku-drive rm '/个人文档/项目/旧数据' --recursive
```

删除通常会移入网盘回收站；若服务器启用了删除审核，命令会显示已提交审核。`/` 和顶级
文档库不能删除。

### JSON、注销和版本

```bash
pku-drive ls '/个人文档/项目' --json
pku-drive status --json
pku-drive logout
pku-drive version
```

## 更多文档

- [完整参考与故障排查](docs/troubleshooting.md)
- [API 与兼容性说明](docs/api-notes.md)
- [安全策略](SECURITY.md)
- [参与贡献](CONTRIBUTING.md)

本项目不是北京大学或爱数官方软件。
