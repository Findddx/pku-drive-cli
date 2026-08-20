# 参与贡献

欢迎提交 Issue 和 Pull Request。协议、认证、数据写入或命令行为的较大变更，建议先在 Issue
中说明使用场景、兼容性和安全影响。

## 开发与检查

开发环境需要 Go 1.22、GNU Make 和支持 race detector 的 C 编译工具链。提交前运行：

```bash
make test
CGO_ENABLED=0 go test ./...
git diff --check
```

行为变更应配套单元或协议测试，并同步更新 README 或 `docs/`。提交只包含本次修改需要的
文件，保持 commit 信息清晰。

## 测试数据与秘密

测试 fixture 使用虚构的账户、对象 ID、token、共享链接、证书和路径。请勿提交真实凭据、
浏览器数据、OAuth 回调 URL、Cookie、link token、签名对象 URL 或个人文件名。错误信息与
测试日志也不能包含这些内容。

Release 中的系统 SPKI pin 是通过独立渠道核验后分发的公开信任锚；普通测试 fixture 使用
本地测试证书和测试 pin。

提交即表示贡献内容按本仓库的 [MIT License](LICENSE) 授权。
