# 北大网盘 API 与兼容性说明

本文记录 `pku-drive` 实际实现依赖的公开协议。它不是抓包记录：没有使用已登录浏览器、
Cookie、Local Storage 或真实会话数据，也不包含 token、client secret、Authorization
值、签名对象 URL、完成 XML 或原始认证响应。

安装和基本命令见[简明 README](../README.md)，登录与故障处理见[完整参考](troubleshooting.md)。

## 公开依据

- [北京大学北大网盘服务说明](https://its.pku.edu.cn/service_1_webdisk.jsp)：确认服务入口
  `https://disk.pku.edu.cn`、北京大学统一身份认证账户及 Linux 客户端。
- [AnyShare OAuth 2.0](https://developers.aishutech.com/napi/documents/167)：授权码、
  refresh token 和客户端认证流程。项目早期调查还记录了同站文档编号
  [240](https://developers.aishutech.com/napi/documents/240)，当前实现以 `/167` 的公开流程
  和本地协议测试为准。
- [AnyShare 文档访问 API 7](https://developers.aishutech.com/napi/documents/307)：文档库、
  目录、秒传、对象上传、multipart、授权刷新及完成接口；旧版同类入口为
  [176](https://developers.aishutech.com/napi/documents/176)。
- [北大实例公开客户端清单](https://disk.pku.edu.cn/api/deploy-manager/client/package)：用于
  确认公开发布的 Linux 客户端版本，不涉及认证。

此外，仅把下载的
`北大网盘2.0_All_Linux_x64-7.0.6.3-20250716-Terminator-281.deb` 当作数据做过静态检查，
未安装或执行。公开文档、该静态检查和无秘密的本地 TLS fixture 共同用于确认字段差异；
以下协议字段并非全部逐项在生产环境实测。

## OAuth 控制面

| 方法与路径 | 用途与实现形状 |
|---|---|
| `POST /oauth2/clients` | 动态注册；JSON 包含客户端名、`authorization_code` / `refresh_token` grant、`code` response、`offline openid all` scope、loopback redirect URI 和 Linux CLI metadata。 |
| `GET /oauth2/auth` | 浏览器授权；query 含 `client_id`、`redirect_uri`、`response_type=code`、scope、随机 `state`，默认再含 PKCE S256 challenge。 |
| `POST /oauth2/token` | `application/x-www-form-urlencoded`；authorization-code exchange 或 refresh-token grant；动态客户端 ID/secret 使用 HTTP Basic。 |
| `POST /oauth2/revoke` | form 中传待撤销 token，动态客户端使用 HTTP Basic；普通 logout 最多分别处理 refresh/access token。 |

回调只绑定 `127.0.0.1`，严格匹配路径、`state` 和一次性 code。只有 token endpoint 明确返回
PKCE 不受支持时才重试无 PKCE 流程，`state` 校验不降级。所有服务端 URL 必须是无
userinfo、query、fragment 的绝对 HTTPS URL。

## 文档与账户控制面

所有下列 AnyShare 请求使用 OAuth Bearer；收到一次 `401` 时刷新凭据并只重放一次。

| 方法与路径 | 请求或响应要点 |
|---|---|
| `POST /api/eacp/v1/user/get` | 当前账户；兼容姓名字段 `name` / `username`。 |
| `GET /api/efast/v1/entry-doc-lib?direction=asc&sort=doc_lib_name` | 可见顶级文档库。 |
| `GET /api/efast/v1/folders/{escaped-id}/sub_objects?limit=1000[&marker=…]` | 目录分页；响应 `dirs`、`files`、`next_marker`。重复 marker 会失败。 |
| `POST /api/efast/v1/file/getinfobypath` | JSON `{namepath}`，保留的适配器兼容接口；当前公共 CLI 路径解析通过入口与分页列举完成，没有生产调用点。 |
| `POST /api/efast/v1/dir/create` | JSON `{docid,name,ondup:1}`；不自动改名。 |
| `POST /api/efast/v1/file/metadata` | JSON `{docid,rev?}`，上传后按精确身份核验。 |
| `POST /api/efast/v1/file/osdownload` | JSON `{docid,rev,savename,authtype:"QUERY_STRING",usehttps:true}`；为精确文件修订取得短时 HTTPS GET 签名。 |
| `POST /api/efast/v1/file/delete` | JSON `{docid}`；按精确文件 ID 移入文档库回收站。HTTP 200 表示已删除，202 表示已进入管理员审核。 |
| `POST /api/efast/v1/dir/delete` | JSON `{docid,check_upload_process:true}`；按精确目录 ID 删除整棵目录树，并要求服务端检查进行中的子上传。HTTP 200/202 语义同文件删除。 |

对象响应兼容 `id` / `docid`；类型缺失时 `size=-1` 作为目录提示。修改时间兼容微秒整数
`modified` 和 RFC3339 `modified_at`，后者存在时优先；`custom_metadata.client_mtime` 存在时
优先于顶层 `client_mtime`。目录列表在客户端合并分页并稳定排序。

## 上传控制面

| 方法与路径 | 精确形状与约束 |
|---|---|
| `POST /api/efast/v1/file/osoption` | 无 body；裸响应 `{partminsize,partmaxsize,partmaxnum}`。 |
| `POST /api/efast/v1/file/predupload` | `{length,slice_md5}`；裸响应 `{match}`。 |
| `POST /api/efast/v1/file/dupload` | 仅新文件秒传：`{crc32,docid,length,md5,client_mtime,name,ondup:1}`；绝不发送 `editedrev`。 |
| `POST /api/efast/v1/file/osbeginupload` | 新文件发送父 `docid`、`name`、`ondup:1`、`length`、`client_mtime`、`reqmethod:"PUT"`；覆盖发送现有文件 `docid` 与原始 `editedrev`，省略 name/ondup。响应含 `authrequest,docid,name,rev`。 |
| `POST /api/efast/v1/file/osinitmultiupload` | 与 begin 的新建/覆盖身份规则相同，但不虚构 `reqmethod`；响应含 `docid,name,rev,uploadid`。 |
| `POST /api/efast/v1/file/osuploadpart` | `{docid,rev,uploadid,parts:"first-last"}`；范围为包含两端的 `1..10000`，响应 key 是十进制分片号。 |
| `POST /api/efast/v1/file/oscompleteupload` | `{docid,rev,uploadid,partinfo}`；`partinfo` 按分片号数值排序，每项为 `[etag,size]`。响应是 multipart，含签名请求 JSON 与需要逐字节保留的 XML。 |
| `POST /api/efast/v1/file/osuploadrefresh` | `{docid,rev,length,multiupload,reqmethod?}`；单次上传带 `reqmethod:"PUT"` 并返回新 `authrequest`，multipart 返回 `uploadid`。 |
| `POST /api/efast/v1/file/osendupload` | `{docid,rev,crc32,md5,slice_md5,editedrev?}`；完成响应可能不回显 `docid` / `rev`，客户端保留请求身份。 |

`csflevel` 是可选字段；没有值时省略，不发送数值零。协议的 `ondup` 中 `1` 表示冲突、
`2` 表示自动改名、`3` 表示替换/合并；本工具只在新建时使用 `1`，覆盖采用已解析文件 ID
和原始 `editedrev`，不会使用自动改名。根据安全决策，已有文件即使命中秒传也不走
`/dupload`，而是 begin/init、对象 PUT、`osendupload` 的覆盖路径。

## 签名对象存储数据面

控制面可能把签名请求编码成旧式数组 `[method,url,"Header: value",…]`。上传路径只接受
HTTPS `PUT`，下载路径只接受 HTTPS `GET`；两者都按第一个冒号拆 header，并拒绝 Bearer
Authorization、HTTP、畸形 header 和任何重定向。PUT 会增加精确 `Content-Length`；GET
使用 `QUERY_STRING` 授权且不转发控制面附带的 header，禁用透明压缩、拒绝编码响应并保留
响应长度用于本地完整性检查。OAuth token 从不发送到对象存储主机。

北大部署的对象端点可能返回主机名与有效期正确、但系统 CA 无法建立链的叶证书。显式进程
变量 `PKU_DRIVE_OBJECT_SPKI_SHA256` 可用于任意控制服务器；变量为空且控制面精确为
`https://disk.pku.edu.cn` 时，才按顺序读取当前用户的
`${XDG_CONFIG_HOME:-$HOME/.config}/pku-drive-cli/object-pin.json`、root 管理的
`/etc/pku-drive-cli/object-pin.json`，最后才是标准系统 CA。用户文件沿用本工具的当前用户
所有、`0600` / `0700` 和拒绝符号链接规则；系统目录与文件必须分别为 root 所有的 `0755`
和 `0644`，并通过 dirfd、`O_NOFOLLOW` 与 `fstat` 读取。较高优先级的文件一旦存在但损坏或
不安全就失败关闭，不会静默回退。持久文件不会影响非北大控制面。pin 模式只替换对象数据面
的 CA 链验证，仍逐次校验证书主机名、有效期与 SPKI，并禁用该隔离 transport 的代理；
控制面 TLS 不降级。

单次或分片 PUT 从 `ETag` 取完成标识，缺失时兼容 `Content-MD5`。下载 GET 的响应流只在
HTTP 200 后交给本地原子写入流程，并核验响应长度及文件 metadata 中可用的 MD5。明确的
`RequestTimeTooSkewed`、`ExpiredToken`、`SecurityTokenExpired`、`Request has expired`
以及 Qiniu 的 `expired token` / `token out of date` / `request time too skewed` 才触发
授权刷新；普通 `403` 和 `SignatureDoesNotMatch` 不会被宽泛认定为过期。错误 body 有
64 KiB 上限且只保留净化后的分类，不记录签名 URL、query、Authorization 值或原始 body。

## 已处理的响应差异与验证边界

- `authrequest` 可直接是签名数组；multipart 完成 JSON 部分也兼容外层
  `{authrequest: ...}`。
- multipart 完成响应 JSON MIME 必须是 `application/json`，XML 必须是
  `application/xml` 或 `text/xml`；空、重复、未知 part 均拒绝。
- 完成上传响应可省略 `docid` / `rev`，但不能用服务端缺字段绕过本地已知身份。
- `osdownload` 的 `authrequest` 必须是签名 GET；服务端若返回 PUT、HTTP URL、Bearer
  Authorization 或畸形 header 会被拒绝。QUERY_STRING 模式下其他控制面 header 不会转发到
  对象域，授权信息只来自签名 URL。
- 文件和目录删除均使用一次精确路径解析得到的对象 ID；目录删除要求 CLI 显式
  `--recursive`。删除是非幂等请求，网络结果不明确时直接返回错误，不重放也不凭名字路径
  猜测已经删除。
- 所有 JSON 响应都有体积上限；上传流程的非幂等请求若连接在提交后丢失，会保存无秘密的
  durable phase 并先按远端精确身份协调，不盲目重放。
- 本地完整协议测试使用彼此独立的 TLS 控制面和对象面，覆盖 refresh、429/503、签名过期、
  响应丢失、完整性不匹配、中断及续传；测试证书只加入测试专用 root pool，从未使用
  `InsecureSkipVerify`。

本文件中的协议断言以公开文档、静态客户端证据和可重复的无秘密测试分别标注；模拟测试
用于验证实现边界，不替代部署方对生产环境兼容性的持续确认。
