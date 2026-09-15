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
| `backend/services/coupon-service` | 券域：券类型、模板、批次、用户券 | `panda_coupon` |
| `backend/services/coffee-machine-service` | 设备域：厂商与接入凭据、咖啡机设备与支付方式、饮品与供应关系、设备事件与余额流水 | `panda_coffee_machine` |
| `backend/services/order-service` | 订单域：订单与订单行、支付分摊、售后单 | `panda_order` |
| `backend/services/payment-service` | 支付域：渠道与支付方式配置、支付单与资金行、渠道回调、记账流水 | `panda_payment` |
| `backend/services/account-service` | 资产账户域：福卡余额与不可变流水（发放 / 扣减 / 冲正） | `panda_account` |
| `backend/services/gateway-service` | 全站唯一入口，单二进制单路由表 | 无 |
| `contracts/` | proto 定义与已提交的生成代码 | — |
| `migrations/` | 八套迁移：`Legacy`（单库时代 001–009，冻结）、`Identity`、`Merchant`、`Coupon`、`CoffeeMachine`、`Order`、`Payment`、`Account` | — |

嵌套的 `go.mod` 是**独立模块**：在 `backend/` 里跑 `go test ./...` 不会碰到八个
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

`panda_coupon`（coupon-service）
: `coupon_types` `coupon_templates` `coupon_template_scopes` `coupon_batches`
  `user_coupons` `user_coupon_scopes` `coupon_redemptions` `coupon_state_transitions`
  `coupon_inventory_ledger` `coupon_idempotency_keys` `message_outbox` `message_inbox`

券域的商户 / 品牌 / 门店 / 用户 / 员工 / 订单 ID 也只存值不建外键：它们分属商户库与
身份库，`REFERENCES` 跨过去就是拆库没做完。**这套表的不变式落在唯一索引和触发器上，
不靠调用方自觉**：一张券同时只能有一条 `succeeded` 的核销（`coupon_redemptions_one_live_success`
部分唯一索引），核销本身按 `request_id` 唯一；一张表表达不了的重试语义走
`coupon_idempotency_keys` 的 `(scope, idempotency_key)` 唯一键并把第一次的 `response`
存下来，重放回放旧响应而不是重算。库存变动只经 `coupon_inventory_ledger`
（`reserve` / `issue` / `release` / `expire` / `adjust`，`quantity <> 0`），
`coupon_batches` 上的三个计数列用 check 约束卡住 `issued + reserved <= total`。
`coupon_types.code` 是稳定的业务标识，改它会被 `coupon_types_code_immutable` 触发器拦下。

`panda_coffee_machine`（coffee-machine-service）
: `manufacturers` `manufacturer_credentials` `devices` `device_payment_methods`
  `drinks` `device_events` `device_balance_ledger`
  `message_outbox` `message_inbox`

`drinks` 一行就是「某台设备上的一杯」：`device_id` 直接挂在这一行上（可空，`ON DELETE
CASCADE`），价格与上下架都在同一行，没有单独的设备×饮品关系表。判重键因此是
`(device_id, manufacturer_id, origin_id)` 的部分唯一索引（`origin_id <> ''`），同一款饮品
在 N 台设备上就是 N 行。

`panda_coffee_machine` 是这几套里唯一**自带资金写入口**的：`devices.coffee_balance`
与 `device_balance_ledger` 记的是咖啡余额，所以那张流水表只允许追加（触发器拦 UPDATE /
DELETE），冲正走反向记录而不是原地改数，幂等靠 `request_id` 上的部分唯一索引。设备通过
`store_id` 关联部署点位、饮品通过 `manufacturer_id` 关联厂商，两者都只存值不建外键——
`store_id` 指向的 `stores` 在商户库，`REFERENCES` 跨过去就是拆库没做完。设备侧
**不持有 `merchant_id`**，商户归属从点位一侧推。

不建外键不等于不校验。写设备（新建与编辑）时本服务会调 merchant-service 的 `GetStore`
确认这个点位存在且未被停用，问不到就拒写并回 503——**fail-closed，不是「校验不了就
跳过」**，那样校验只在商户服务正常时才生效，等于没有校验。「不存在」与「问不到」在
服务层是两个不同的结果：前者是调用方填错了（400），后者不是（503）。

授权这条链（§5.2.3 的操作员 → 授权点位 → 点位下设备）本服务**不发事件、不维护投影**，
将来按 §5.2.4 允许的另一条路走：查询时实时向持有那一方的服务取关系。选它的理由是
后台设备页人少，实时调用换掉一份投影表和它的一致性维护（事件延迟、遗漏、服务重启，
§5.2.4 对这些都有要求）更划算。代价是列表接口必须先在数据层限定范围再分页，且依赖
问不到时按「权限归属未知」拒绝访问，不能退化成看全部。挂载前的点位校验走的是同一个
模式，客户端都在 `internal/client/`。

这条边界不只是表归属，还有一条对外契约：**厂商接入凭据与令牌由 coffee-machine-service
唯一持有并签发**。出杯由 fulfillment-service 直连厂商执行（§5.11），但厂商账号、`access_token`
与刷新过程都收敛在本服务——`manufacturer_credentials` 是唯一权威副本，fulfillment 调用前
向本服务取一组只读材料：`access_token` / `token_expires_at` / `api_base_url` / `signing_secret`。
两个适配器各自脱敏、各自记 Provider 调用流水，只有凭据不分散：同一份密钥配两处，就会出现
两边刷新时机不一致、互相把对方刚刷出来的令牌刷废。刷新那一行加行锁、留提前刷新窗口、
失败**保留旧令牌**而不是清空。**签名密钥也在交付之列**——出杯回调的验签在 fulfillment
侧做，漏了它回调一律验不过。

`panda_order`（order-service）
: `orders` `order_lines` `order_payment_lines` `order_state_transitions`
  `order_after_sales` `order_idempotency_keys` `message_outbox` `message_inbox`

订单库里的用户 / 门店 / 设备 / 饮品 / 支付方式 ID 同样只存值不建外键。订单**不复制支付事实**：
支付单号、出资行都落在支付库，订单侧只保留自己那几列（`orders.payment_no`、
`order_payment_lines`），它们由 `payment.succeeded` / `payment.failed` 事件写进来——支付库那边
改了什么，订单这边不镜像，只按事件结算一次。

`panda_payment`（payment-service）
: `payment_channels` `payment_methods` `payments` `payment_fundings` `payment_refunds`
  `payment_refund_fundings` `payment_notifications` `payment_provider_calls`
  `payment_transactions` `payment_state_transitions` `payment_idempotency_keys`
  `payment_agreements` `payment_agreement_charges` `reconciliation_batches`
  `reconciliation_records` `message_outbox` `message_inbox`

资金的**真相只有一处**：`payments` 以及它下面的资金行。订单服务不复制它，只收两条结果事件；
反过来支付服务也**不读订单库**（§5.9 给它的职责清单里没有「读订单」）——金额与归属由
order-service 在锁内校验完，通过 gRPC 把权威金额交过来（见下面的服务间契约表）。
渠道密钥不进库：`payment_channels` 只存 `secret_ref`（密钥在受控 Secret 或环境变量里的**键名**），
`payment_provider_calls` 存的是脱敏摘要。这与 `manufacturer_credentials` 直接存密钥列的先例
**有意不一致**：那张表先于 §18.3 存在，支付库是新建的，按 §18.3 建。

`payment_methods.action` 是**数据**，不是代码里的分支：它描述「这条支付方式被选中之后怎么起
支付」（跳对方小程序 / 小程序内 requestPayment / 直扣 / 扫码 / H5 / 账户出资），客户端只
switch 它、不 switch 渠道代码。新接一家同形态的渠道 = 插一行 `payment_channels` + 一行
`payment_methods` + 注册一个适配器，客户端与表约束都不用动。接真实渠道的差别被关在
`internal/provider/` 的一个包里。

两条边界值得单独说明：

- **`merchant_users` 在身份库**，因为它是账号域（凭据、状态、范围），且所有写入方
  都在 user-service。merchant-service 只是需要知道「这个商户下还有没有账号，能不能
  删」——那是一句话的问题，走 RPC，不共享表。
- **`admin_operation_logs` 在身份库**，审计由身份侧拥有。它刻意不对目标对象建外键，
  这样目标被删除后日志仍可查询；拆库前它指向 `merchants` 的那条外键已经去掉，列保留
  为软指针。

`panda_account`（account-service）
: `fortune_card_accounts` `fortune_card_entries` `fortune_card_freezes` `message_outbox`
`message_inbox`

福卡是**抽奖凭证**，不是出资渠道：账户域只有余额与流水两件事，没有「福卡价值多少元」这种
列，也不参与任何一笔订单的金额计算。用户 ID 只存值不建外键（它在身份库）。

这套表和 `panda_coffee_machine` 的 `device_balance_ledger` 是同一种东西——余额 + 只追加
流水 + 幂等键 + 冲正——所以约束也照它建：`fortune_card_entries` 用触发器拦 UPDATE / DELETE，
冲正写一条反向记录而不是原地改数（`reverses_entry_id` 自引用，部分唯一索引保证一笔只能被
冲正一次），`entry_key` 唯一索引保证重投不重复发放。余额行**懒创建**：第一次发放时
`INSERT ... ON CONFLICT DO NOTHING`，随后 `SELECT ... FOR UPDATE` 锁住再算余额——
并发扣减靠这把锁串行化，不靠乐观重试。

账户域**不持有发放规则**：订单承诺几张、拆成几条流水，都由 order-service 决定后随
`order.completed` 事件传过来（见下面的服务间契约）。这正是 §5.6「Account 只拥有账户和账变
数据」那条边界的落点——规则将来会变（加赠活动、会员权益），账户域不该跟着变。

**冻结不是账变。** 用户提交退款申请的那一刻起，那一单送出的福卡不能拿去抽奖
（`fortune_card_accounts.frozen_balance` + `fortune_card_freezes`，一张售后单一行，
`after_sale_no` 是幂等键）。余额与流水一格不动——`balance_after` 记的是当时真实的余额，
把冻结写进去反而会让明细页对不上账；能拿去抽奖的是 `available = balance - frozen_balance`，
只有抽奖扣减看它。冻结是个**可改的状态行**，所以 `fortune_card_freezes` 是全库唯一一张
不带只追加触发器的资产表。

要冻哪几笔由 order-service 按退款范围拆好（退饮品行给 `base` 那张，退加购行给
`bonus:{campaignId}` 那张，整单退给全部），账户域不猜「哪张卡是哪一行送的」。拆不动时
**宁可多冻**：多冻可解冻，少冻则追不回——已经抽过奖的福卡是追不回来的，这条规则存在的
全部理由就在这里。

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
| coffee-machine-service → merchant-service `GetStore` | 设备挂点位前确认这个点位存在且可用 |
| order-service → coffee-machine-service `GetDevice` | 下单前确认这台设备在不在、启没启用、点位对不对（§5.8 L454） |
| order-service → payment-service `CreatePayment` | 发起支付：订单侧在锁内校验完归属、状态与金额，把**权威金额**交给支付域建支付单 |

除 gRPC 之外还有一类跨服务事实，它走的是消息而不是调用：**福卡的发放与退款冻结**。

| 发布方 → 消费方 | 事件 | 用途 |
| --- | --- | --- |
| order-service → account-service | `order.completed` | 订单完成后发放福卡（`base` + 各加购活动的 `bonus`，每条带幂等键） |
| order-service → account-service | `order.after_sale.applied` | 用户申请退款 ⇒ 冻结这一单的福卡，附上要冻的幂等键 |
| order-service → account-service | `order.after_sale.reviewed` | 审核**驳回** ⇒ 解冻；通过不解冻（钱还没退） |
| order-service → account-service | `order.after_sale.cancelled` | 用户撤销申请 ⇒ 解冻（放弃撤销入口之后唯一的解冻信号） |

这四条挂在同一个队列上（订阅侧一个队列绑多个 routing key，逗号分隔）——它们都是
account-service 与订单域之间的交接点，散到几个队列只会让「少绑了一条」变成一个安静少做的事。

发放规则与账户分开是有意的：承诺几张、拆成几条，由 order-service 从订单自己的
`fortune_cards_expected` 与快照里算出来随事件传过去；account-service 只记流水，不解析快照、
不判断该发几张。快照缺失或算不平**不缩水**——退回一条 `base`，金额取订单承诺的总数，
用户拿到的张数永远等于订单承诺的数，退化只发生在流水的构成粒度上。

冻结时点是**申请**而不是审核通过：申请到审核之间有一个窗口，卡在这个窗口里被花掉就再也追
不回来。`paid` 状态就允许申请退款，所以冻结可能早于发放到达——发放落库时会反查有没有覆盖
这个键的冻结行并补冻，这条补齐路径是必需的，不是兜底。

这条事件今天由后台的「标记完成」产生（`paid → completed` 那条边本该由履约完成事件驱动，
而 fulfillment-service 还没建）。两者发的是同一个 `order.completed`，所以履约接上来时下游
一个字都不用改。账户域**不发下游事件**：发放结果今天没有消费者（抽奖没建），发出去只会被
静默丢弃。

最后一条与其余几条有一点不同：它是一次**写**（建支付单）而不是读一个事实。它成立的前提是
归属分得清——金额与「这单能不能付」的判据在订单库里，只有订单服务说得清；而支付单、渠道
配置、回调验签在支付库里，只有支付服务碰得到。所以是订单侧编排、支付侧执行，不是反过来。
它走同步 gRPC 而不是消息：客户端发起支付后要立刻拿到支付参数，异步那条路给不了。

`order/v1` 与 `coupon/v1` 两个 proto 目前仍是空壳（`service X {}`，一个 rpc 都没有）。
那不是「这两个域没实现」——它们的 HTTP 入口都已经在跑——而是**还没有一条需要固化成契约的
内部调用**。空壳在这里只是「还没被依赖」，等第一个调用方出现再往里加 rpc；现在写了也没人验。
`fulfillment` / `inventory` / `lottery` / `membership` / `partner` / `settlement`
才是真的没有实现。

`account/v1` 这个目录里有两个不相干的东西，别把它们当成一件事：`account.proto` 是后台与商户端
**登录账号**的草稿，全仓没有一处 import；`fortune_card.proto` 是资产账户域，已经实现。
后者的 `GetFortuneCardBalance` / `DeductFortuneCards` / `ReverseFortuneCardEntry`
**今天没有调用方**（抽奖没建、退款单没建），它们不是死代码而是这个服务的对外契约——
集成测试直接走 gRPC 客户端打这三个。

coffee-machine-service 两头各占一半：作为**调用方**它问商户服务的点位，作为**被调方**它回答
订单服务的设备查询。写入、同步与出杯那几条链路还没实现。

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
- 审计走 outbox：业务事务里同时写 `message_outbox`，relay 异步投递到 RabbitMQ。
  这是**唯一**允许延迟的一类，走这条路的今天只有 `admin.operation.logged`（纯通知）。
  审计是跟着写入走的旁路记录，投递慢一拍不影响业务状态本身。

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

转发面里**唯一一条从公网打进来的路径**是渠道回调 `POST /v1/payments/callback/{channelCode}`。
它挂在 `/v1/payments` 这个独立前缀上，不站在 `/v1/miniapp` 那棵小程序树下：后者已经整段转给
订单域与 user-service 了，而且回调来自渠道而不是来自小程序。这条路径**刻意不挂认证**——渠道
没有我们的令牌，它的凭据是自己的签名，验签在支付服务里做，失败的回调一点都改不了支付状态。
因为没有认证这一层，网关的限流就是它唯一的把关，所以别把它挪到限流链外面。

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

## 小程序用户与它的后台入口

C 端顾客共 4 张表，都在 `panda_identity`：`users`（账号主体，手机号可空且唯一）、
`user_wechat_identities`（开放平台里的一个用户会有多个应用的 openid，所以拆表而不是
在 `users` 上加两列）、`user_sessions`（**只存 refresh token 的哈希**——这张表泄露时，
明文 token 等于可以直接冒用登录态）、`user_login_events`（登录安全事件，刻意不对
`users` 建外键，理由与 `admin_operation_logs` 相同：账号注销后失败记录仍要可查）。

**后台权限码与平台管理员分开**：`admin:miniapp-users:view` / `:manage`，不复用
`admin:users:*`。能管平台管理员不等于该看到每一个小程序顾客的手机号，复用一个码等于把
两批人的可见范围绑死。这也是**新造权限码**的少数正当场合之一（对照「图片上传」那条：
那里不新造码是因为既有码本来就蕴含同样的授权，这里不是）。

- 禁用走 `PATCH /v1/admin/miniapp-users/{id}/status`，在**同一个事务**里改
  `users.status`、撤销该用户全部有效会话（`revoked_at=NOW()`，`revoke_reason='disabled'`）、
  往 outbox 落一条审计事件（由本服务的消费者写进 `admin_operation_logs`）。撤销语句是
  照抄的，没调 `RevokeUserSessions`——后者用连接池、会把自己的事务提交在管理事务之外。
- **启用不撤销任何会话**（他手里本来就没有活的会话），响应里的 `revokedSessions` 为 0；
  禁用时这个计数就是界面提示「踢下线 N 个登录态」的来源。
- `deleted` 是用户自己注销留下的终态，后台改不了，改就是 409：后台没有「复活」入口。
- 审计载荷里的手机号是**脱敏**的（`139******06`）：审计事件经 outbox 递到 RabbitMQ，
  完整的手机号到那里等于多存了一份 PII。
- 列表里 `status=deleted` 是合法的筛选值、却是非法的设置值，所以筛选与设置的校验各用
  一个错误值——共用一个的话，筛选写错会提示「只能为 active 或 disabled」，而 `deleted`
  明明能筛出结果。

## 操作日志的读取方

审计事件从产生到能看，走完整条链路：
**业务事务内写 `message_outbox`** → relay 投递到 RabbitMQ → user-service 的消费者落
`admin_operation_logs` → 后台页面查 `GET /v1/admin/operation-logs`。

这条链路以前只有前半截：表在写、有消费者、有幂等键，但**没有任何读取方**，日志实际
存在却看不见。加读取方时有两个位置选择：

- **查询留在 user-service**，不新起一个「日志服务」。`admin_operation_logs` 属于身份
  库，审计消费者（也就是这张表唯一的写入方）本来就住在这里，查询另起上游等于让一个
  库有两个所有者。仓储是**一个实例服两个入口**：消费者写、后台页面读。
- **写入侧一个字没改**。列表接口是纯只读的，没有 UPDATE / DELETE，也不会有「清空
  日志」——权限码因此只有 `admin:operation-logs:view`，**刻意不造 `manage`**。有
  `manage` 就意味着存在后台能改审计记录的状态，而这张表的完整性正是它的全部价值。
  留存多久是 DBA 按留存策略处理的事，不由页面决定。

**`occurred_at` 是落库时间，不是操作发生的时刻——这是这条链路的一个真实缺口。**
`audit.Entry` 和 `messaging.Envelope` 都不带时间戳，列取的是消费者 INSERT 时的
`NOW()`。直连链路下两者差约一秒，看起来没问题；但 relay 积压后补投的那一批会整体晚于
实际操作时间（实测补投时看到过 04:10:50 的行对应 ~04:04:28 的操作）。**排查时如果发现
「日志时间和操作时间对不上」，先看中间有没有积压，不要怀疑时钟。** 要修得给
`audit.Entry` 加时间戳并一路透传到 INSERT——那会同时改动所有审计调用点，属于另一件事，
这轮没做。

列表的筛选项（模块/动作/结果/操作人/时间范围）与分页共用同一份 WHERE 与参数构造，
`FindPage` 与 `Count` 不允许各持一份分支——两者错开的症状只在翻到第二页之后出现。
筛选项的候选值走单独的 `GET /v1/admin/operation-logs/facets`（`SELECT DISTINCT`），
**不在前端写死一份**：模块名由各服务的审计调用点决定，写死的那份会在下一个模块上线时
静默少一项，而「筛选里没有我要找的模块」看起来就是日志没记上，排查方向完全错了。

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
- **支付只做「发起 → 回调 → 结算订单」这一条闭环**。退款、委托代扣、渠道对账三件事的表
  已经在 `panda_payment` 里建好，但没有代码也没有 worker；真实渠道适配器（微信、银联、
  丰选万联、优联、北方饭卡、首创饭卡）一个都没写——没有商户凭据时写出来只能验参数构造，
  那不是验证，是猜。本轮只有手工渠道（`provider=manual`），它做**真的** HMAC 验签，所以
  「验签失败不改支付状态」与「重放被唯一键挡住」是被真验到的。账户出资（咖啡豆走
  `action=account`）**已落地一条纯豆全额的路**：豆账户在 account-service 里（§5.6 的
  另一半，余额 + 只追加流水、单位分、永不过期、不冻结），后台人工调整是它唯一的来路
  （`POST /v1/admin/coffee-beans/{userId}/adjustments`，`account:manage`），发起支付时同步
  调用扣减、没有 `pending` 中间态（钱在自家库里，扣成功就是成功）。**这一条路目前只支持
  「全额用豆付」**：混合出资（豆 + 渠道凑一单）没做；payment-service 的退款单也没做——纯豆
  单的退款由 account-service 自己在售后审核通过时冲正，见下面福卡那一段。生产环境的咖啡豆
  支付方式也还没有：只有 `deploy/dev-seed/payment_coffee_bean_method.sql` 一行，服务端没有
  支付方式管理页，上线要人工配。福卡不在出资词表里——它是下单赠送的抽奖凭证，不是出资渠道
  （order/003、payment/004 已收窄）。
- **福卡到「发放 + 退款冻结」为止**：余额、不可变流水、发放/扣减/冲正、订单完成自动入账，
  以及退款申请期间的冻结与驳回/撤销后的解冻。**退款成功后的追回没做**——那要等
  payment-service 的退款单，届时是「解冻 + 冲正那两笔发放」两件事，先后顺序得先解冻
  （否则撞 `frozen_balance <= balance`）；今天售后停在 `approved`，所以「通过不解冻」是
  有意的，不是漏了。**豆那一半相反：审核通过（`approved`）就冲正**，两者不矛盾——福卡是
  还没花出去的凭证，钱没离开账户，解冻等于放它回去继续花；豆是支付时就已经收下的钱，还
  回去才是退款。渠道支付的订单走到豆的冲正这条路上会什么都不发生（找不到那笔扣减），那
  是常态不是异常。抽奖（lottery-service 的活动、参与、开奖、中奖、核销）也没建，
  `DeductFortuneCards` 仍然没有调用方——冻结落地后它判「够不够扣」看的第一次是**可用**
  余额，这是冻结唯一的判据改动。服务端**不计算**承诺福卡——`fortuneCardsExpected` 仍采信
  调用方，与「价格来自调用方」是同一个已知缺口；加购加赠活动的规则归属也仍未定，本轮只是
  把它的快照原样记进流水。人工冻结/解冻没有后台入口（冻结只有「售后申请」一个触发源），
  也没有补冻的定时兜底——事件丢了就是丢了，与发放同一条取舍。`miniapp/` 前端仍是空目录
  （余额与冻结字段已经在接口里回来了，页面下一轮）。
- **支付方式列表接口不做**：「这台设备能用哪几条方式」是设备域 `device_payment_methods`
  的事实，归设备域；支付服务不为它开一条从支付库读咖啡机库的口子。
- **后台/商户端的支付页面不做**：`/v1/admin/payments` 这类前缀留给后台切片，admin-web 与
  merchant-web 一行都没改。
