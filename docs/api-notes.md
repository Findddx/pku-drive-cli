# 北大网盘 API 与兼容性说明

本文面向维护者，记录 `pku-drive` 使用的 OAuth、AnyShare 文档与分享链接协议，以及客户端
必须保持的验证边界。安装与命令见[项目 README](../README.md)，运行故障见
[故障排查](troubleshooting.md)。

## 协议依据

- [北京大学北大网盘服务说明](https://its.pku.edu.cn/service_1_webdisk.jsp)：服务入口、统一
  身份认证和 Linux 客户端信息。
- [AnyShare OAuth 2.0](https://developers.aishutech.com/napi/documents/167)：授权码、refresh
  token 和动态客户端流程。
- [AnyShare 文档访问 API 7](https://developers.aishutech.com/napi/documents/307)：文档库、目录、
  分享链接、上传、下载与授权刷新接口。
- [北大实例公开客户端清单](https://disk.pku.edu.cn/api/deploy-manager/client/package)：公开
  Linux 客户端版本信息。

兼容性判断还参考官方 Linux 客户端包的静态资源，以及不含真实账户数据的本地 TLS 协议
fixture。测试数据使用虚构 token、对象 ID、URL 和证书。

## OAuth 控制面

| 方法与路径 | 用途 |
|---|---|
| `POST /oauth2/clients` | 动态注册 CLI；grant 包含 `authorization_code`、`refresh_token`，scope 为 `offline openid all`。 |
| `GET /oauth2/auth` | 浏览器授权；携带 loopback `redirect_uri`、随机 `state` 和默认的 PKCE S256 challenge。 |
| `POST /oauth2/token` | 交换授权码或刷新 access token；动态客户端凭据使用 HTTP Basic。 |
| `POST /oauth2/revoke` | 注销时撤销 refresh token 与 access token。 |

回调监听绑定 `127.0.0.1`，并严格验证回调路径、`state` 和一次性 code。服务端明确拒绝 PKCE
时才以相同安全状态重试兼容流程。配置中的服务端地址必须是没有 userinfo、query 和 fragment
的绝对 HTTPS URL。

凭据按用户保存在受权限约束的 XDG 配置目录。普通 API 请求携带 OAuth Bearer；首次收到
`401` 时刷新 token 并重放一次。

## 账户、目录与下载

| 方法与路径 | 请求或响应要点 |
|---|---|
| `POST /api/eacp/v1/user/get` | 当前账户；姓名兼容 `name` 与 `username`。 |
| `GET /api/efast/v1/entry-doc-lib?direction=asc&sort=doc_lib_name` | 当前账号可见的顶级文档库。 |
| `GET /api/efast/v1/folders/{id}/sub_objects?limit=1000[&marker=…]` | 目录分页；客户端合并 `dirs`、`files` 并检测重复 marker。 |
| `POST /api/efast/v1/file/getinfobypath` | `{namepath}` 兼容接口；主要路径解析通过入口和逐级列举完成。 |
| `POST /api/efast/v1/dir/create` | `{docid,name,ondup:1}`；父 ID 来自精确路径解析。 |
| `POST /api/efast/v1/file/metadata` | `{docid,rev?}`；传输前后核对精确文件身份。 |
| `POST /api/efast/v1/file/osdownload` | `{docid,rev,savename,authtype:"QUERY_STRING",usehttps:true}`；返回短时签名 GET。 |
| `POST /api/efast/v1/file/delete` | `{docid}`；HTTP 200 为已删除，202 为进入审核。 |
| `POST /api/efast/v1/dir/delete` | `{docid,check_upload_process:true}`；处理完整目录树，HTTP 200/202 语义同上。 |

对象 ID 兼容 `id` 与 `docid`；文件夹类型在内部规范为 `directory`。修改时间兼容微秒整数
`modified` 和 RFC3339 `modified_at`；`custom_metadata.client_mtime` 优先于顶层
`client_mtime`。目录项目在分页合并后稳定排序。

删除使用一次路径解析得到的对象 ID。目录删除需要 CLI `--recursive`；提交后的网络结果不
明确时由调用者重新列目录确认，客户端不自动重放非幂等删除。

## 分享链接会话

分享链接入口限定为控制面同源的 `https://disk.pku.edu.cn/link/{link_id}`。输入不能携带
userinfo、query、fragment 或跨域跳转，link ID 只接受有限长度的字母数字串。

打开链接的流程如下：

1. 使用无 Cookie、无 OAuth 且拒绝重定向的客户端请求
   `GET /api/shared-link/v1/links/{link_id}`，取得链接类型、项目类型、有效期和访问限制。
2. `anonymous` 分享再使用新的内存 Cookie jar 请求 `/link/{link_id}`，验证所有重定向仍在
   同一 HTTPS origin，并核对落地页信息与规范元数据一致。
3. 从落地响应提取短时 `link_token:{link_id}` 后丢弃浏览 Cookie 会话，使用独立 Bearer 会话
   调用
   `GET /api/efast/v1/entry-item` 精确匹配分享根对象。
4. `realname` 分享跳过落地页，使用用户已经建立的 OAuth 会话；缺少登录或访问权限时返回
   认证类错误。
5. 文件夹继续使用 `sub_objects` 分页，文件使用 `file/metadata` 与 `file/osdownload`。下载
   对象流沿用相同的完整性检查和本地安全写入流程。

共享会话只列举当前相对目录的一层内容，进入子目录时再请求下一层。相对路径拒绝开头或结尾
斜线、空段、`.`、`..`、反斜线与 NUL；解析每一级都要求名称唯一且对象类型匹配。

批量下载先解析并预检全部目标，再按输入顺序逐文件传输。每个文件独立校验并原子发布；后续
文件失败时保留已经完成的文件，并在稳定错误消息中给出完成数量。

短时 link token、Cookie 和签名 URL 仅保存在当前进程内存，作用域限制到同一 HTTPS origin。
服务端声明 `password_required`、`verify_mobile` 或过期时间时，分别返回稳定错误分类；当前 CLI
由用户通过网页处理提取码和手机验证。

## 上传控制面

| 方法与路径 | 形状与约束 |
|---|---|
| `POST /api/efast/v1/file/osoption` | 返回 `partminsize`、`partmaxsize`、`partmaxnum`。 |
| `POST /api/efast/v1/file/predupload` | `{length,slice_md5}`；返回秒传匹配状态。 |
| `POST /api/efast/v1/file/dupload` | 新文件秒传：`{crc32,docid,length,md5,client_mtime,name,ondup:1}`。 |
| `POST /api/efast/v1/file/osbeginupload` | 新建发送父 ID、名称、长度与 `reqmethod:"PUT"`；覆盖发送文件 ID 和原始 `editedrev`。 |
| `POST /api/efast/v1/file/osinitmultiupload` | 初始化 multipart；新建与覆盖沿用相同身份规则。 |
| `POST /api/efast/v1/file/osuploadpart` | `{docid,rev,uploadid,parts:"first-last"}`；分片号范围为 `1..10000`。 |
| `POST /api/efast/v1/file/oscompleteupload` | `{docid,rev,uploadid,partinfo}`；分片按数值排序，响应含签名请求和完成 XML。 |
| `POST /api/efast/v1/file/osuploadrefresh` | 刷新单次或 multipart 上传授权。 |
| `POST /api/efast/v1/file/osendupload` | `{docid,rev,crc32,md5,slice_md5,editedrev?}`；提交并核验最终 metadata。 |

`csflevel` 有有效值时才发送。新建使用 `ondup:1` 报告冲突；覆盖绑定已经解析的文件 ID 与
revision。已有文件即使命中秒传也走覆盖协议，避免把秒传结果绑定到错误对象。

multipart 续传状态保存本地文件指纹、远端身份、upload ID、分片 ETag 和 durable phase。
发生连接丢失时先协调远端精确状态，再决定继续或报错。

## 签名对象存储

控制面可把对象请求编码为 `[method,url,"Header: value",…]`。上传只接受 HTTPS `PUT`，下载
只接受 HTTPS `GET`；解析器拒绝重定向、HTTP URL、畸形 header 和 Bearer Authorization。
OAuth token 不会发往对象存储主机。

下载采用 `QUERY_STRING` 授权，不转发控制面 header，并禁用透明压缩。HTTP 200 响应流先
核验内容长度和 metadata 中可用的 MD5，再由本地原子写入器发布。上传从 `ETag` 读取完成
标识，缺失时兼容 `Content-MD5`。

北大对象端点可使用发行包中的 SPKI pin。控制面为 `https://disk.pku.edu.cn` 时，读取顺序是
用户配置 `${XDG_CONFIG_HOME:-$HOME/.config}/pku-drive-cli/object-pin.json`、系统配置
`/etc/pku-drive-cli/object-pin.json`、标准系统 CA。pin 模式继续验证主机名、证书有效期和
SPKI，并为对象面使用隔离 transport；控制面始终使用系统 CA。

明确的签名过期响应会触发一次授权刷新。其他 `403` 保持服务端拒绝语义。错误 body 设有
64 KiB 上限，只输出净化后的分类，不记录 query、Cookie、Authorization 或原始响应。

## 测试边界

本地协议测试使用相互独立的 TLS 控制面与对象面，覆盖 OAuth refresh、匿名与实名分享、
目录按需浏览、429/503、签名过期、响应丢失、上传续传、完整性不匹配和中断清理。测试证书
只加入测试专用 root pool。

协议字段变化时，应先添加最小 fixture 固化服务端响应，再更新兼容解析；新的兼容分支需要
保持同源校验、响应体积上限、精确对象身份和秘密净化规则。
