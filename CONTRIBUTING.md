# 参与贡献

欢迎提交 Issue 和 Pull Request。开始修改前，请先搜索已有 Issue；较大的协议或行为变更建议
先开 Issue 说明使用场景和安全影响。

开发环境需要 Go 1.22、GNU Make 和可用于 race detector 的 C 编译工具链。提交前运行：

```bash
make test
CGO_ENABLED=0 go test ./...
git diff --check
```

贡献应包含与行为变化对应的测试，并保持错误输出不泄露秘密。测试 fixture 必须使用虚构的
账户、对象 ID、token、URL、证书和路径；不要提交真实凭据、浏览器数据、OAuth 回调、签名
对象 URL或个人文件名。Release 中的系统 SPKI pin 是经过独立核验后公开分发的信任锚；普通
测试 fixture 不应复制生产证书或 pin。

提交即表示贡献内容按本仓库的 [MIT License](LICENSE) 授权。
