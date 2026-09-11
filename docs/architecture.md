# panda_v2 架构

本文记录 panda_v2 的运行时结构：库边界、服务间契约、一致性语义，以及两条容易被
误用的机制（策略广播、边缘限流）的边界。它是「为什么这样拆」的记录，不是逐文件
的说明——那部分看代码和 `contracts/proto`。

## 模块与进程

| 模块 | 角色 | 自有存储 |
| --- | --- | --- |
| `backend/` | 平台库（config、server、client、messaging、cache、ratelimit、upload、observability…） | — |
| `backend/services/user-service` | 身份域：账号、角色、权限、菜单、审计 | `panda_identity` |
| `backend/services/merchant-service` | 商户域：商户、品牌、门店、审核记录 | `panda_merchant` |
| `backend/services/gateway-service` | 全站唯一入口，单二进制单路由表 | 无 |
| `contracts/` | proto 定义与已提交的生成代码 | — |
| `migrations/` | 三套迁移：`Legacy`（单库时代 001–008，冻结）、`Identity`、`Merchant` | — |

嵌套的 `go.mod` 是**独立模块**：在 `backend/` 里跑 `go test ./...` 不会碰到三个
服务模块。CI（`.github/workflows/check.yml`）对每个模块各跑一遍
`gofmt` / `go build` / `go vet` / `go test`，就是为了不再出现「服务从未被构建过」。

## 库边界

`panda_identity`（user-service）
: `admin_users` `admin_roles` `admin_permissions` `admin_role_permissions`
  `admin_user_role_bindings` `admin_menus` `admin_role_menus` `casbin_rule`
  `merchant_users` `admin_operation_logs` `message_outbox` `message_inbox`

`panda_merchant`（merchant-service）
: `merchants` `brands` `stores` `brand_audit_records` `store_audit_records`
  `message_outbox` `message_inbox`

两条边界值得单独说明：

- **`merchant_users` 在身份库**，因为它是账号域（凭据、状态、范围），且所有写入方
  都在 user-service。merchant-service 只是需要知道「这个商户下还有没有账号，能不能
  删」——那是一句话的问题，走 RPC，不共享表。
- **`admin_operation_logs` 在身份库**，审计由身份侧拥有。它刻意不对目标对象建外键，
  这样目标被删除后日志仍可查询；拆库前它指向 `merchants` 的那条外键已经去掉，列保留
  为软指针。

**不再有跨库外键。** 迁移里出现 `REFERENCES` 跨到另一个库的表，就是拆库没做完。

`scope_id`（账号范围）本来就是多态指针、没有外键，拆库后维持原样。

## 服务间契约

服务之间只走 gRPC，定义在 `contracts/proto/`。生成的 `.pb.go` **提交进仓库**，所以
构建不需要本机装 buf/protoc；CI 里另有一个 job 重新生成并对比，防止生成物漂移。

当前真实在用的内部调用，全部是「读一个事实」或「执行一个动作」两类：

| 调用方 → 被调方 | 用途 |
| --- | --- |
| merchant-service → user-service `HasUsers` | 商户下还有账号吗（决定能否删除） |
| merchant-service → user-service `ResetAccountScope` | 删除品牌/门店后回收账号范围 |
| merchant-service → user-service `AdminAccessService.GetAdminAccess` | 转发管理员的授权判定 |
| user-service → merchant-service `GetBrandMerchant` / `GetStoreMerchant` | 账号范围指向的实体归属 |
| user-service → merchant-service `ResolveScopeNames` | 把 scope 上的 id 换成名字 |

其余 proto（coupon、order、payment…）目前是占位服务，没有实现，不构成契约。

`platform/client.Dial` 决定用静态地址还是服务发现：注册中心真的实现了
`Resolver`+`Watcher` 就走 discovery，否则退回静态地址。**这不是「发现失败就退回静态
地址」**——注册中心配了却连不上，会 fail loudly，不会悄悄绕过自己。

## 一致性语义

**同步调用 + fail-closed，不做最终一致。** 跨库的写操作走同步 RPC，对端不可用就整体
失败并返回可重试的错误，而不是「先删了再说，回头补」。

具体的失败语义，按操作分三种：

- 删除品牌/门店前要回收账号范围。回收调用失败 → 整个删除失败，**本地不删**。宁可
  这次删不掉，也不要留下「实体没了、账号范围还指着它」的状态。
- 角色/权限保存后要刷新 Casbin 快照。刷新失败 → 返回「已保存但未生效，请重试」。
  变更已经在库里了，但**不假装它生效了**。
- 审计与通知事件走 outbox：业务事务里同时写 `message_outbox`，relay 异步投递到
  RabbitMQ。这是**唯一**允许延迟的一类，因为它承载的是通知/审计，不是状态。

`RABBITMQ_URL` 为空时 `NewRabbitMQ` 返回 Noop。platform/server 特意**不给 Noop 发布器
配 relay**：Noop 会接受每一次投递并回报成功，配上 relay 等于把每条事件标成「已投递」
再丢掉，比不投更糟。这时事件堆在表里，等服务接上 broker 再投。

## 策略广播的边界

`user-service` 每个副本都在内存里持有一份 Casbin 快照（鉴权中间件走的是内存，不是
每次查库）。多副本时，A 副本改了角色权限，B 副本会继续按旧规则鉴权。

处理方式是广播「该重读了」，不是广播规则：

- 提交变更的副本先本地 `Reload()`（保持「同一请求内生效」的语义），再往
  `panda:policy:changed` 发一条**只含自己实例标识**的消息。
- 其它副本收到后各自 `Reload()`，即自己去库里读最新的。**消息里没有规则内容**，
  所以不存在「广播里的规则」和「库里的规则」不一致这种状态。
- 发布方按实例标识跳过自己的消息。实例标识用随机 UUID 而不是注册中心的实例 ID：
  唯一的要求是进程间不重复，而随机值不依赖地址配置（`GRPC_ADDR` 为空时 kratos 会
  自己挑端口，两个副本可能算出同一个注册 ID 而互相跳过通知）。

`REDIS_ADDR` 为空时退化为 `cache.Noop`：`Reload` 仍在本实例生效，只是不广播。多副本
部署下这会让副本之间短暂不一致，所以生产必须配 Redis。

**这条链路不缓存任何鉴权结果。** 广播的只是规则快照；每一次鉴权仍然实时走内存里的
快照判定，Redis 里没有、也不会有 grant。

## 限流与转发头的边界

限流放在网关上，因为那是全站唯一的入口，拦在边缘才不会让超量请求先穿过代理再被每个
服务各自挡一遍。

- 限流键是 `RemoteAddr` 的客户端 IP + 路由类别（`auth|` / `api|`）。**刻意不看
  `X-Forwarded-For`**：那是请求头，客户端想写什么就写什么，用它当键就等于把限流开关
  交给对方（每次换个值就换一个桶）。
- 认证接口（`/auth/login`、`/auth/refresh`）有单独且更紧的额度，撞库最集中的就是
  这两个入口；两类各记各的键，登录额度不会被普通流量提前耗掉。
- Redis 可用时是共享的滑动窗口（多副本共享一份额度）；`REDIS_ADDR` 为空时退化为
  进程内令牌桶，**额度变成每副本一份**。
- 限流器自身出错时**放行**。限流是一层保护，不该在它自己出问题的时候变成把所有人
  挡在外面。

网关在转发前会**重写**（不是追加）`X-Forwarded-For/-Proto/-Host`。`httputil` 的
`SetXForwarded` 是追加语义，会把客户端伪造的值留在链首；下游按「第一个」取来源时
拿到的就是伪造值。同时所有身份相关头（`x-user*`、`x-tenant*`、`x-roles`、
`x-service-token`）一律剥掉——没有任何下游还信任它们。

## 图片上传与区划

图片走**后端 → OSS**，浏览器不直传：直传要下发 STS 临时凭据或预签名 URL，等于把写
权限和签名逻辑放进前端；而上传本来就要经过 admin 的实时鉴权链。上传入口是
`POST /v1/admin/uploads/images`（multipart，字段名 `file`），实现在
`backend/platform/upload`（配置、校验、key 与 URL 构造，零 SDK import）+ `oss.go`
（唯一 import aliyun 的文件）。

- **键名沿用旧后端 panda_serve 的**（`OSS_ACCESS_KEY` / `OSS_SECRET_KEY` /
  `OSS_ENDPOINT` / `OSS_BUCKET` / `OSS_CNAME` / `UPLOAD_PATH` / `UPLOAD_MAX_FILE_SIZE` /
  `UPLOAD_USE_MD5`），所以「搬配置」就是抄四行值。只搬键名，值一律空占位——
  `deploy/config/.env.example` 里不出现真值。**配置缺失不阻塞启动**：merchant-service
  照常起来，只有上传接口返回 503 并点名缺哪个变量（条件注册路由会让同一二进制在不同
  环境有不同路由表，网关转发到 404 时响应里没有任何线索指向配置）。
- **key 强制是内容寻址的 md5**（`<UPLOAD_PATH>/images/<md5[:2]>/<md5[2:4]>/<md5><ext>`）。
  同一份字节两次上传得同一个 key，所以**重试天然幂等**——客户端超时重传不会留下第二个
  对象。`UPLOAD_USE_MD5=false` 只打印一条警告、不生效：按客户端文件名拼 key 时
  `filename="../../../x.png"` 能写到 `UPLOAD_PATH` 之外，而 `filepath.Join` 不清理 key。
- **不信客户端的 Content-Type**：按字节嗅探，并把嗅探结果写成对象的 `Content-Type`。
  否则对象以 `application/octet-stream` 落库，浏览器点开是下载而不是渲染。图片白名单
  是**代码里的**规则，不是运维能放宽的 env。`.svg` 被拒（`image/svg+xml` 满足
  「以 image/ 开头」，但它是可执行 XML）。
- **鉴权复用 `admin:brands:manage` / `admin:stores:manage` 两个既有码**（`protectedAny`），
  不新造权限码：新码意味着身份库迁移 + 给现有角色补授权，漏一步就只有超管能上传。
  这比裸认证强——只读管理员拿不到往公开 bucket 写的能力，而能合法贴图的人本来就持有
  这两个码之一。链路仍过 `authorizer.middleware`，被回收权限的人实时被挡。
- **超时三层，由外到内收紧**：网关 `GATEWAY_UPLOAD_TIMEOUT_MS` 120s > merchant-service
  `HTTP_TIMEOUT_MS` 90s > OSS SDK `readWrite` 60s。网关**不动**全局
  `GATEWAY_REQUEST_TIMEOUT_MS`——那会让任意一条卡住的商户路由都把网关 goroutine 钉住
  120s；改的是按路径选 timeout。120s 而不是 60s：这段预算覆盖「客户端上行 + OSS 写入」，
  10MiB 走 1Mbps 上行约 80s，60s 会稳定产出 504。反之 SDK 必须有上界：旧实现把 `ctx`
  传给 SDK，而 `PutObject` 不认 context，卡住的 PUT 能挂 200s——服务端已返回失败、
  goroutine 还在写，客户端被告知失败而对象可能写成了。

**省市区同时存名字与编码**（`stores.province/city/district` +
`province_code/city_code/district_code`）。名字是给人看的，编码是给机器定位的，两者不是
冗余：数据源会过期——`NewCoffee` 自带的 `pca-code.json` 里至今挂着 2021 年已撤销的
「下城区」，只有 `district='下城区'` 时没人能确定性地把它迁到新的拱墅区，有 `330103`
才能识别、对照官方变更记录迁移。而且**编码事后算不出来，名字随时能从编码推出来**；
全国区县还有 28 组重名，只有「省/市/区」三元组能定位。历史行是自由文本（「北京」而非
「北京市」），回填工具 `cmd/backfill-region` 尽力而为、**匹配不上就留空并计数**，不假装
全量；表单里解析不出来的旧值原样保留，绝不因为打开一次编辑就把老数据洗成空。

区划数据源是 `element-china-area-data@6.1.0`（与 `coffeevoucher_admin` 同版本），前端在
`packages/ui` 里用它生成级联选项，后端回填工具是 Go、读不了 npm 包，所以由
`pnpm --filter @panda-v2/ui regions:export <file>` 导出一份 JSON 当入参——**不提交这份
副本**，它从提交那刻就开始漂移。数据只覆盖 31 个省级，**不含港澳台**。

## 可观测性

`observability.Init` 在配了 `OTEL_EXPORTER_OTLP_ENDPOINT` 时建 OTLP 导出器并设置
OTel 全局 provider；没配时返回全局的空实现，不建立任何网络连接。

- `platform/server` 给 HTTP 与 gRPC server 都挂上 `tracing.Server` 与
  `metrics.Server`（kratos 自带，不引入新依赖）。注意 `metrics.Server()` 在没拿到
  计数器/直方图时是**静默空操作**，所以两个都必须建出来。
- `platform/client.Dial` 在客户端侧挂 `tracing.Client`：服务端 Extract、客户端
  Inject，只挂一边跨服务的 span 就接不起来。
- 自定义计数器走 `observability.Counter`，它取的是 otel **全局** meter。全局 meter
  可后置委派：在 `Init` 之前建出来的 instrument 会在 provider 就位时自动接上，所以
  组件可以在 `main` 里先构造、后初始化。目前有 outbox 投递/失败、Casbin 重载
  （按 local/remote 分开）两个。

**接线有两个反直觉点，都踩过，改之前先读这段**：

- **HTTP 路由必须经 `runtime.HTTPRouter` 注册，不能只挂 `khttp.Middleware`。**
  kratos 只在 protoc-gen-go-http 生成的 handler 里查 middleware matcher，而本仓库
  每个路由都是裸 `Server.HandleFunc`——没有任何代码去问 matcher，中间件就成了
  **静默空操作**：不报错、不产生 span、不产生指标。`HTTPRouter` 在注册时把链包在
  handler 外面，这是唯一能覆盖全部路由的位置。包在外面是安全的：kratos 的 mux
  filter 在 handler 执行前已经把 transport 装进请求 context，tracing/metrics 正是
  从那里读 operation 与 path 模板。另外 `http.HandlerFunc` 不返回 error，所以
  `HTTPRouter` 会用 `statusRecorder` 记下状态码、在 ≥400 时**重建**一个 kratos
  error 交给链（只为让指标读到 code，响应已写出，该 error 被丢弃）。
- **`auth.GRPCServerOption` 走 `Server.Use("/*", ...)` 而不是 `kgrpc.Middleware`。**
  kratos 的 `matcher.Use` 是**赋值**（`m.defaults = ms`）不是追加，而
  `GRPCServerOptions` 排在 runtime 自己的 option 之后应用——用 `Middleware` 会把
  runtime 的插桩整个顶掉，同样表现为「没有 span、没有指标、也不报错」。`Use` 写的是
  prefix 表，与 defaults 叠加，且让 runtime 中间件留在最外层（被拒的调用也照样进
  trace）。两处都有回归测试：`platform/server/runtime/httprouter_test.go` 与
  `platform/auth/grpc_test.go`，去掉修复它们会失败。

**指标比 trace 慢一拍**：span 走 BatchSpanProcessor（5s 一批），指标走
`PeriodicReader`（默认 **60s** 一次）。所以做完操作后 tempo 几秒内就能看到 trace，
而 prometheus 里的计数器最多要等 60s 才出现；计数器的数据点也是**有 measurement
才产生**的——没触发过重载 / 没投递过事件时，`panda_policy_reloads_total`、
`panda_outbox_published_total` 在 prometheus 里「无样本」是正常现象，不是没接上。
排查前先用 `curl :8889/metrics`（collector 的 prometheus 导出器）确认，它能直接区分
「服务没上报」和「prometheus 没抓到」。

**未做（有意）**：限流拒绝的计数器。它落在网关上，而网关目前唯一的非标准库依赖是
限流必需的 go-redis；再加 OTel SDK 会引入计划之外的第二处依赖，因此留待单独决定。

## 明确不做的

- **不拆网关**：单二进制、单路由表。
- **不做最终一致**：跨库写同步调用，失败关闭。
- **不缓存鉴权结果**：Redis 只承载规则广播与限流窗口。
- **不做省市区字典表与下发接口**：今天唯一的消费者是后台一个表单，做出来只是把同一份
  JSON 从 `packages/ui` 挪到后端，换来新表 + 约 3445 行迁移 + 新接口 + 网关新路由 +
  表单加载态。等**第二个消费者**（小程序改用同一份主数据）出现再做。
- **上传不堵「手填任意 URL」**：`brands.logo` 等列是无类型 text，创建/更新路径不校验
  URL。服务端 URL 校验是另一个改动，且会让存量行失效——要有意识地决定，别默认「做了
  上传就等于修好了」。
- **不做图片删除与生命周期**：被拒的上传、放弃的表单都会在 bucket 里留下对象（md5
  去重只对**完全相同**的字节生效）。这属于 OSS 生命周期规则，不该由应用代码扫。
