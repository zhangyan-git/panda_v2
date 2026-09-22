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
| `backend/services/payment-service` | 资金域：支付单与资金行、渠道回调、记账流水——**「收钱的方式」不是它的数据，是 `internal/catalog` 里的常量表**（009 删掉了 `payment_channels` / `payment_methods` 两张表）；退款 / 对账 / 分账 / 结算单也归它。这四件事的表都建好了（退款与对账在 001/002，分账与结算在 008）；分账**建任务**那一刀已落地（发起支付时命中规则、算出金额、写出任务与接收方），退款 / 对账 / 结算单、以及向渠道发起分账仍**一行代码都没有**（见「分账与结算的归属」） | `panda_payment` |
| `backend/services/account-service` | 资产账户域：福卡余额与不可变流水（发放 / 扣减 / 冲正） | `panda_account` |
| `backend/services/lottery-service` | 抽奖域：门店开通、抽奖活动与奖池、期次与参与、开奖与中奖记录 | `panda_lottery` |
| `backend/services/membership-service` | 会员域：套餐、会员资格与有效期、变更记录 | `panda_membership` |
| `backend/services/partner-service` | 开放域：合作方账号与 API 密钥、开放接口入站（`/v1/openapi/*` 唯一入口）、两条设备回执通路（刷卡机与取货码） | `panda_partner` |
| `backend/services/gateway-service` | 全站唯一入口，单二进制单路由表 | 无 |
| `contracts/` | proto 定义与已提交的生成代码 | — |
| `migrations/` | 十一套迁移：`Legacy`（单库时代 001–009，冻结）、`Identity`、`Merchant`、`Coupon`、`CoffeeMachine`、`Order`、`Payment`、`Account`、`Lottery`、`Membership`、`Partner` | — |

嵌套的 `go.mod` 是**独立模块**：在 `backend/` 里跑 `go test ./...` 不会碰到十一个
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
DELETE），冲正走反向记录而不是原地改数，幂等靠 `request_id` 上的部分唯一索引。

那个写入口今天只有一个：`DeductDeviceBalance`（取货码那条路，`type='deduct'`；后台充值 /
调整走另一条、写 `adjust`）。它把**扣款与写流水放在同一个事务**里——老系统那条路只有一次
`$inc`、一分钱流都不留，这一处是有意不照抄。**幂等的判据是那条唯一索引，不是先查后插**：
短路读只让重投不白跑一次，真正的守门人仍是索引。短路读还带来一个运维上必要的性质——**重投
在验证码校验之前就被认出来**，所以「钱扣了、单没建出来」的那一笔可以拿对方单号补建，而不
受后台改码影响（合作方视角的拒绝档与各自该做什么见 `docs/openapi.md` §6.3）。

**「扣了没建单」有一个人工出口**：`POST /v1/admin/orders/pickup-repairs`（`order:manage`，
后台页面上没有按钮，与设备余额调整那条同一种东西）。它带的是**空取货码**，而空码过不了那台
设备的校验，所以它**不可能引起一次新的扣款**；反过来说，它回「取货码不对」时的意思是
**这笔钱从来没扣过**——那一单不该补，先去查合作方到底有没有发起过。重复补同一单不会建出
第二张（建单那一步按对方单号命中既有单）。设备通过
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
: `payments` `payment_fundings` `payment_refunds` `payment_refund_fundings`
  `payment_notifications` `payment_provider_calls` `payment_transactions`
  `payment_state_transitions` `payment_idempotency_keys` `payment_agreements`
  `payment_agreement_charges` `settlement_rules` `settlement_rule_items`
  `settlement_receivers` `settlement_accounts` `settlement_tasks` `settlement_reversals`
  `settlement_statements` `settlement_statement_items` `reconciliation_batches`
  `reconciliation_records` `message_outbox` `message_inbox`

资金的**真相只有一处**：`payments` 以及它下面的资金行。订单服务不复制它，只收两条结果事件；
反过来支付服务也**不读订单库**（§5.9 给它的职责清单里没有「读订单」）——金额与归属由
order-service 在锁内校验完，通过 gRPC 把权威金额交过来（见下面的服务间契约表）。

**收钱的方式不建表。** `migrations/payment/009` 删掉了 `payment_channels` 与
`payment_methods`：用户能选的四种方式跟着代码走。**前三种**（咖啡豆 / 银联商务小程序 /
银联商务 H5）写在 `internal/catalog` 的常量表里；**第四种取货码不在那张表里**——钱在这台
设备的咖啡余额里，由 partner-service 验签后转 order-service 直接落 `paid`，从不经过支付
服务（见 `order-service/internal/service/create_pickup.go`）。早先它们是运营数据——后台两页、`payment:manage` 一枚码、
渠道行的 `config` 里二三十个协议键（签名算法、成功码、时间戳格式），那要求运营先当协议工程师。
现在它们跟着代码走，`Method.ChannelCode` 只用来写日志。

渠道密钥不进库这条规矩没变，只是**今天这个库里一个密钥列都没有**：账户值在
`internal/catalog` 里拼成一棵配置树（可以被打印），两把对称密钥走 `Channel.SecretEnv`——
存的是**环境变量名**，值在签名那一刻才 `os.Getenv`；`payment_provider_calls` 存的是脱敏摘要。
（`manufacturer_credentials` 直接存密钥列是另一回事：那张表先于 §18.3 存在，支付库是新建的。）

`catalog.Method.Action` 也不是库里的数据（跳对方小程序 / 小程序内 requestPayment / 直扣 /
扫码 / H5 / 账户出资），它描述「这条支付方式被选中之后怎么起支付」，客户端只 switch 它、
不 switch 渠道代码。接一家新渠道 = 写一个实现 `provider.Provider` 的包 + 在 catalog 里加一行
+ 在 `.env` 里加一组变量，客户端与表约束都不用动（同族的渠道共用一份实现，见下面支付那一段）。

分账、分润规则与结算单**不另建服务**，也落在这个库：它们与支付单是同一条资金链上的另几段。
表已经建好（`migrations/payment/008_settlement_core.sql`：配置三张、执行三张、结算两张），
三条边界（权限码、冻结快照、依赖方向）与那个已经补上的前置契约，记在下面的「分账与结算的归属」。

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

福卡是**抽奖凭证**，不能出资：账户域只有余额与流水两件事，没有「福卡价值多少元」这种
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

`panda_lottery`（lottery-service）
: `lottery_activations` `lottery_campaigns` `lottery_campaign_prizes` `lottery_rounds`
  `lottery_participations` `lottery_draws` `lottery_wins` `lottery_win_events`
  `message_outbox` `message_inbox`

抽奖域**只拥有抽奖与奖品数据**（§5.7）：福卡余额在账户库，订单事实在订单库，门店与设备在
商户库与设备库，四者一律只存不透明 UUID（订单与设备另存一份参与时的展示快照）。所以「这个
用户有几张卡」在抽奖库里查不到——参与时实时调 account-service 的 `DeductFortuneCards`，把
回来的 `entry_id` 存成值引用。

**门店名没有快照**：`lottery_activations` 原先有一列 `location_name`（开通那一刻的名字），
2026-09-15 去掉了（`migrations/lottery/003`）。它换不来什么，却让「显示当前店名」和「按门店名
搜」两件事只能二选一——名字一旦不落库，`ILIKE` 那条筛选在 SQL 里就做不了，而商户域的 gRPC 也
没有「按名字查门店」（`ListStores` 只收 merchant_id）。留的是实时查，换掉的是那个交互：后台的
筛选改成从门店下拉里选一家（传 `location_id`，走本来就有那条等值比较），名字由
`ResolveScopeNames` 一页解一次。顺带把「幽灵门店」堵掉了：开通前先 `GetStore` 要一次存在性，
不存在的门店回 404，而不是把一条查无此店的开通记录建出来。名字解不出来时**只让名字空着**
（记一条 warn），不让整页打不开——展示的降级不该拦住「停用一家店的抽奖」。

**订货 / 库存域已从 V2 删掉**（2026-09-22）。原先这里有 `panda_inventory` 与一整套
「结余 + 只增流水」的表（`materials` / `warehouses` / `stock_levels` / `stock_movements` /
入库单与出库单…），连同 `backend/services/inventory-service`、`migrations/inventory/`、
`contracts/proto/inventory/`、网关那条上游与后台七页一起删了；身份库里那三个菜单与四枚权限码
由 `migrations/identity/036` 收回。**这一门域将来要单独拆一个服务出去**，所以不是「先摘入口、
接口留着」——留在这里的只有那条判断本身，将来重做时按它走：库存是「结余 + 只增流水」的形状，
`stock_movements` 带只追加触发器、且有一条指向 `stock_levels` 的复合外键，所以写的次序钉死
（先 upsert 结余、再写流水）；作废不是删单，原流水一个字不动、另写一条反向的。
`panda_inventory` 那个库与它的数据**还在 dev 卷上**，只是不再有任何服务连它
（`admin_operation_logs` 里那条 `inventory / confirm` 同理留着，见前端的 operationLogText）。

`panda_membership`（membership-service）
: `membership_plans` `memberships` `membership_subscriptions` `membership_changes`
  `message_outbox` `message_inbox`

会员是**在别处成交、在这里生效**：买卖会员那张单是 `panda_order` 里一条
`line_type='membership'` 的订单行，会员价的优惠落在 `panda_coffee_machine` 的饮品价格列上，
本库只有两样东西——**资格**（`memberships`：谁、哪一档、到什么时候）与**会员价的发券配置**
（每期发几张、哪个模板）。所以这里**没有次数表**：包月会员的会员价靠会员价券发出去，与老系统
同一套做法。

**快照列是成交那一刻的拷贝**，不是套餐的实时引用：`plan_code` / `plan_name` / `member_price_*`
都在开通时从订单行上的套餐快照原样落进 `memberships`，套餐事后改价、改时长、下架都不改写已经
买了的人拿到什么。发券按快照发，不回头现查套餐——现查等于让一次后台编辑改写所有在途订单。

一个用户同时只有一条会员（`memberships_user_unique`），续期是在这一行上往后叠日历而不是新开
一行，所以 `renewal_count` 才有意义。`membership_changes` 是只追加的变更流水（触发器挡
UPDATE / DELETE），`(order_id, change_type)` 上的唯一索引是**重投的幂等凭据**：支付成功回调会被
重放，第二次撞上它时仓储回 `ErrDuplicateChange`，服务把它翻成 ack——没有这条索引，一次重投就是
把会员续了两期。

**签约表还只是表**：`membership_subscriptions` 与它的两条唯一索引（一人一条生效中的订阅、一份
代扣协议只对应一条）已经建好，但签约、代扣、解约一个都没实现——套餐能卖、会员能开通，连续包月
的**扣款**链路停在 DDL 上。`membership_plans.wechat_plan_id` 在 `auto_renew` 时必填，那是给这条
链路留的位置。

**开通是挂门店的一个动作，活动再分粒度。** `lottery_activations` 一个门店一行
（`UNIQUE (location_id)`），活动通过 `activation_id` 挂上去，设备级活动靠
`lottery_campaigns.machine_id` 非空表达——**不用 `scope_type` + `scope_id` 两列**，那样
「某台咖啡机的活动不属于本门店」在结构上就写得出来，而这种写法要靠服务层自觉。开通有操作人
和时间，不是从「有没有启用中的活动」派生出来的：派生会让门店在期次之间的空档里显示成
「没开通」。

**期次是滚出来的，没有「建一期」的接口**：活动一开第一期就出来了，开奖的同事务里开出下一期，
**作废一期也在同一事务里补开下一期**（作废之后活动不能没有在跑的期次）。一期只能开一次的全部
依据是 `lottery_draws.round_id` 上的**唯一索引**——多副本 worker 因此不需要选主，抢输的那个
INSERT 冲突即可。`lottery_rounds` 上还有一条部分唯一索引
`(campaign_id) WHERE status IN ('open','closed')`，保证一个活动同时只有一期在收人。

**期次与活动都没有时间窗口**（2026-09-15 删掉，迁移 `004`）：一期**只有收满门槛**才会自动开奖，
没满就一直开着等，没有任何东西会因为时间到了把它开掉。所以一个 `open` 的期次不是「一直没被扫到」，
而是**有意停在那里**，它的出路只有人工开奖或作废。开奖只有 `threshold` 与 `manual` 两个 trigger，
`lottery_draws.trigger` 的 CHECK 与之逐字一致。

`participant_count` 是**存下来的列**而不是 `COUNT(*)`：它既是抽奖中心的渲染要读的数，又是开奖
worker 的扫描判据。所以它和 `SUM(amount) = balance` 一样是要被测的不变式。并发下它靠
`UPDATE ... SET participant_count = participant_count + 1 WHERE id = $1 AND status = 'open'`
这一句串行化：`READ COMMITTED` 下行锁会重读最新版本再算 `SET`，没有「先读后写」的窗口。
达标即把期次置 `closed` 停止收人，与 `status='open'` 那个条件一起构成「扣减飞行途中期次关了」
的唯一检出点——影响 0 行就走冲正把卡退回去。

开奖**可复核但不可证明公平**：种子是派生值而非随机数，
`sha256(round_id ‖ 首个参与 id ‖ 末个参与 id ‖ 参与数 ‖ trigger)`；中奖名单是对每条已确认参与算
`sha256(seed ‖ participation_id)` 升序取前 N 名（`algorithm = 'sha256-sort-v1'`），插入顺序不影响
结果。种子与参与集合都落库，所以第三方可以把名单完整重算一遍。**这不是 commit–reveal**：有库读
权限的运维在「最后一人参与到扫描之间」能预测结果，那不在本轮范围。

奖品兑付**不调 coupon-service**：`coupon.proto` 是空壳，发券只有要管理员 actor 的
`POST /v1/admin/coupons/issue`。所以奖品的 `prize_kind` / `coupon_template_id` 今天只存不消费。

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
| order-service → coffee-machine-service `GetDrink` | 下单买饮品时取这一杯的**权威名称、图片与价格**。请求里只有 `item_id`：名称、原价、会员价全由这一份回答填，客户端说了不算（与下面 `GetMembershipPlan` 那条同一个形状）。它**不按 status 过滤**，下架与否由 order-service 判——设备回调那条路读同一列却有意不拦（钱已经在机器上收过了） |
| order-service → coffee-machine-service `DeductDeviceBalance` | 取货码那条路上**唯一一次动钱**：条件扣 + 写一行余额流水，同一个事务，幂等键是对方单号。回答里带回「这一次实际扣掉的金额」——重投时它是当初那一笔，订单要照它记账 |
| order-service → payment-service `CreatePayment` | 发起支付：订单侧在锁内校验完归属、状态与金额，把**权威金额**交给支付域建支付单 |
| order-service → payment-service `CreateRefund` | 发起退款：审核通过之后那一步。**幂等键是售后单号**（`payment_refunds.after_sale_no` 整表唯一），所以重发不会退两次钱。同步等待「建没建成」，退成没退成由事件回来（见「退款闭环」） |
| order-service → membership-service `GetMembershipPlan` | 下单买会员时取套餐的**权威价格与时长**。请求里只有 `plan_id`：价格、名称与会员价路径全由这一份回答填，客户端说了不算 |
| order-service → membership-service `GetMemberPriceEntitlement` | 下单买饮品时问「这个人此刻算不算会员价」。会员价那一列在饮品目录上，**谁有资格按它买**在会员域——两个事实、两个域，本服务一个都没有。答不上来整单回 503，**不退回原价继续下单**（那会把一次下游抖动变成「悄悄按原价卖给了会员」） |
| lottery-service → account-service `DeductFortuneCards` | 用福卡参与抽奖时扣卡。`fortune_card.proto` 的**第一个真实调用方** |
| lottery-service → account-service `ReverseFortuneCardEntry` | 参与在中途被开奖抢先（期次已关）时把卡原路退回 |
| lottery-service → merchant-service `GetStore` | 开通门店抽奖前确认这家门店存在（不存在回 404，问不到回 503） |
| lottery-service → merchant-service `ResolveScopeNames` | 列表页的门店名：抽奖库只存门店 id，名字是**读这一刻**现解的 |
| merchant-service / coffee-machine-service / order-service → user-service `MerchantAccessService.GetMerchantAccess` | 商户请求的数据边界：拿令牌现换一次「这个账号现在能看哪些点位」。**每请求一次、不缓存**，见下节 |
| user-service → merchant-service `ListStoreIDs` | 把 `scope_type`/`scope_id`（merchant / brand / store 三档）展开成一组门店 id。展开带 `merchant_id` 交叉校验 |

除 gRPC 之外还有一类跨服务事实，它走的是消息而不是调用：**福卡的发放与退款冻结**。

| 发布方 → 消费方 | 事件 | 用途 |
| --- | --- | --- |
| order-service → account-service | `order.completed` | 订单完成后发放福卡（`base` + 各加购活动的 `bonus`，每条带幂等键） |
| order-service → account-service | `order.after_sale.applied` | 用户申请退款 ⇒ 冻结这一单的福卡，附上要冻的幂等键 |
| order-service → account-service | `order.after_sale.reviewed` | 审核**驳回** ⇒ 解冻。通过是**空分支**：福卡不解冻、豆也不冲正，钱还没退 |
| order-service → account-service | `order.after_sale.cancelled` | 用户撤销申请 ⇒ 解冻（放弃撤销入口之后唯一的解冻信号） |
| order-service → account-service | `order.after_sale.refunded` | 钱退成了 ⇒ **追回福卡**（解冻 + 冲正这一单那几笔发放，余额真的少掉）**并冲正咖啡豆**。追不回来的部分（申请之前就被抽掉的）按冻结额钳住，写进冻结行的 `reason` |
| order-service → account-service | `order.after_sale.refund_failed` | 钱没退成 ⇒ 解冻，卡原样放回去。**不是收尾工作**：不在这里解开，用户重新申请时在途冻结已经吃光可用，新冻结行只冻得到 0 张，第二次退款成功就一张都追不回来 |

这六条挂在同一个队列上（订阅侧一个队列绑多个 routing key，逗号分隔）——它们都是
account-service 与订单域之间的交接点，散到几个队列只会让「少绑了一条」变成一个安静少做的事。

后两条是退款链在账户域的收口，也是**唯一**两条能让冻结行走完的路：一支结在 `recovered`
（卡收回来了），一支结在 `released`（卡放回去了）。冻结不再有「一直冻着」这个结局。

⚠️ 它们与 `payment.refund.*` 一样是**三段以上**的 routing key，照着旧的那条
`order.completed` 抄绑定不会出错（是整串逗号列表逐条绑），但漏写一条的症状与上面
那个 `payment.*` 的坑一样：发布方报成功、消息静默丢弃，钱退了卡还在，两边日志都干净。

退款结果是反方向的：payment-service 发，order-service 收。它挂在 order-service **已有的
那个队列**上（`order.payment.dev`，见下节），所以订单服务只管一个队列。

| 发布方 → 消费方 | 事件 | 用途 |
| --- | --- | --- |
| payment-service → order-service | `payment.refund.succeeded` | 钱退成了 ⇒ 售后单 `refunding → refunded`（写 `refunded_at`、`refund_no`），订单 `refunding → refunded`（整单）或回到退款前那个状态（按行）；`orders.refunded_amount` 累加 |
| payment-service → order-service | `payment.refund.failed` | 钱没退成 ⇒ 售后单 `refunding → failed`（写 `failure_code` / `failure_message`），订单回到退款前那个状态 |

⚠️ **话题交换机上 `payment.*` 匹配不到 `payment.refund.succeeded`**：`*` 在 AMQP 里匹配
*恰好一段*，而退款事件是三段。绑定写成 `payment.*,payment.refund.*`（见
`deploy/compose/dev/docker-compose.yml`）。漏掉的症状是老坑：发布方报成功、消息静默丢弃，
订单永远停在 `refunding`，两边日志都干净。没有 `refund.processing` 这个事件——不确定的
退款不发事件，售后单停在 `refunding` 本来就是对的（钱确实还没回去）。

会员那条走的是**另一个队列**，因为它跟福卡无关：

| 发布方 → 消费方 | 事件 | 用途 |
| --- | --- | --- |
| order-service → membership-service | `order.paid` | 这一单里有会员行时开通或续期。会员快照由订单侧从 `order_lines.membership_plan_snapshot` 原样搬来（不重新拼、不回头现查套餐），**带没带这一段就是「是不是买会员的单」**——没有它时 ack，那是常态 |

⚠️ membership-service 目前**不在 dev compose 里**，所以它的队列名与绑定没有地方钉：手工起它时
若不显式给 `RABBITMQ_QUEUE` / `RABBITMQ_ROUTING_KEY`（绑 `order.paid`），它会沿用 `.env` 里的
那两行——而那两行可能是**别的消费者**的队列（例如身份审计的 `admin.operation.logged`），于是它
认领并 ack 掉别人的消息，两边都不报错。

发放规则与账户分开是有意的：承诺几张、拆成几条，由 order-service 从订单自己的
`fortune_cards_expected` 与快照里算出来随事件传过去；account-service 只记流水，不解析快照、
不判断该发几张。快照缺失或算不平**不缩水**——退回一条 `base`，金额取订单承诺的总数，
用户拿到的张数永远等于订单承诺的数，退化只发生在流水的构成粒度上。

冻结时点是**申请**而不是审核通过：申请到审核之间有一个窗口，卡在这个窗口里被花掉就再也追
不回来。`paid` 状态就允许申请退款，所以冻结可能早于发放到达——发放落库时会反查有没有覆盖
这个键的冻结行并补冻，这条补齐路径是必需的，不是兜底。

这条事件今天由后台的「标记完成」产生（`paid → completed` 那条边本该由履约完成事件驱动，
而 fulfillment-service 还没建）。两者发的是同一个 `order.completed`，所以履约接上来时下游
一个字都不用改。账户域**不发下游事件**：发放结果今天没有消费者——抽奖域虽然建起来了，但它
按设计**不消费 `order.completed`**：参与是用户拿着福卡主动发起的（§3.1 明确舍弃了「订单完成
自动加入抽奖」），所以抽奖库那边没有 inbox 消费者，`RABBITMQ_QUEUE` 也不配。

最后一条与其余几条有一点不同：它是一次**写**（建支付单）而不是读一个事实。它成立的前提是
归属分得清——金额与「这单能不能付」的判据在订单库里，只有订单服务说得清；而支付单、渠道
配置、回调验签在支付库里，只有支付服务碰得到。所以是订单侧编排、支付侧执行，不是反过来。
它走同步 gRPC 而不是消息：客户端发起支付后要立刻拿到支付参数，异步那条路给不了。

`order/v1`、`coupon/v1` 与 `lottery/v1` 三个 proto 目前仍是空壳（`service X {}`，一个 rpc 都没有）。
那不是「这几个域没实现」——它们的 HTTP 入口都已经在跑——而是**还没有一条需要固化成契约的
内部调用**。空壳在这里只是「还没被依赖」，等第一个调用方出现再往里加 rpc；现在写了也没人验。
lottery 这一头尤其干净：开奖在进程内，没有任何入向 gRPC，所以它是全仓第一个
「**只出向、不入向**」的服务（出向那两条见上面的服务间契约表）。
partner-service 是第二个**只出向、不入向**的服务，理由与上面两条一样：它已经建起来并在跑
（`/v1/openapi/*` 的唯一入口 + 设备刷卡通路），出向调 order / membership / 身份校验，
而 `partner/v1` 还没有一条需要固化的入向 rpc。**真的没有实现的只有 `fulfillment`**；
`settlement` 则**不打算单独建服务**——它并入 payment-service，契约里那行空壳保持空着
（理由与三条边界见「分账与结算的归属」）。`membership/v1` 不在这份空壳名单里：
会员域已经建起来，它的 `GetMembershipPlan` 是订单域下单定价的**唯一**来源（见上面的服务间
契约表）。它现在**只做了开通与续期**——签约、代扣、解约都还没有，四条出向事件（`membership.activated`
等）也还没有消费者：会员价的券要等 coupon-service 那边接上（它今天只有要管理员 actor 的
`POST /v1/admin/coupons/issue`，没有任何消费者）。

它的 HTTP 面两端各有一棵树：小程序那半棵是 `/v1/miniapp/membership` 与它下面的 `/plans`、
`/auto-renew`（只验令牌，控制器按令牌里的 `user_id` 取自己那一条，路径上没有 id 可越权）；
后台那半棵是套餐与会员两个资源，**三枚权限码分开**：`membership:read`（看，迟早要发给客服）、
`membership:manage`（改套餐——只影响接下来卖什么，已购会员有快照）、`membership:adjust`
（冻结 / 解冻 / 撤销 / 改有效期）。把 `adjust` 并进 `manage` 是这里最容易犯的错：那等于让任何
一个能改套餐文案的人也能给人白送一年会员价。

`account/v1` 这个目录里有两个不相干的东西，别把它们当成一件事：`account.proto` 是后台与商户端
**登录账号**的草稿，全仓没有一处 import；`fortune_card.proto` 是资产账户域，已经实现。
后者的 `DeductFortuneCards` / `ReverseFortuneCardEntry` 第一个真实调用方就是 lottery-service
（见上面的服务间契约表），`GetFortuneCardBalance` 还没有人调——退款单也没建。三个 rpc 的
集成测试都直接走 gRPC 客户端打，所以它们不是死代码，是这个服务的对外契约。

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

## 商户端入口与数据范围

商户控制台是 `merchant-web`，只读三屏：门店、设备、订单（列表 + 详情）。**没有饮品**
（饮品管理留在后台那棵 `/v1/admin/coffee-machines` 树上）、**没有员工与角色管理**
（商户账号一律由后台「商户管理」建），也**没有任何写入口**。概览页仍是占位。

三条商户路由，网关按前缀转发，**网关自己不解析身份**：

| 路由 | 服务 |
| --- | --- |
| `/v1/merchant/stores`、`/v1/merchant/stores/{id}` | merchant-service |
| `/v1/merchant/devices`、`/v1/merchant/devices/{id}` | coffee-machine-service |
| `/v1/merchant/orders`、`/v1/merchant/orders/{id}` | order-service |

各服务的中间件链是 `auth.Middleware` → `authz.MerchantMiddleware`。**没有 `RequirePermission`**：
商户域没有权限码（`009` 已删商户角色表），能看见什么**只由数据范围表达**。

### 数据范围的唯一形状：一组门店 id

`merchant_users.scope_type / scope_id` 三档——`merchant`（该商户全部门店）、`brand`、
`store`——**一律展开成一组门店 id**（`StoreIDs`）再往下传。设备表只有 `store_id`，订单表
只有 `store_id`，所以下游只认这一个形状；**不向任何业务表加 `merchant_id`**
（§5.2.3 的约束，加列会让「商户可见性」变成一张表一个说法）。

由此带来一条容易被写错的分界线：**nil 与空切片不是一回事**。

```sql
AND ($2::text[] IS NULL OR store_id::text = ANY($2::text[]))
```

`nil` = 调用方没传过滤条件（后台筛选栏就是这么用的，行为一字不变）；**非 nil 空切片 =
这个账号一个点位都没授权 → `= ANY('{}')` 恒假 → 零行**。老代码里「空即不过滤」那套惯用法
（`coalesce(cardinality($2),0)=0 OR …`）**绝不能**用在数据范围上——一个没授权任何点位的
账号会看到全平台。安全归一化（nil → 空切片的那一步）固定在 `auth.WithStoreScope` 一处。

### 实时取权，不签进令牌

access token 的 TTL 是 24 小时。把 scope 快照签进去意味着改一次数据范围要等一天才生效，
所以商户域**每个请求都现取一次**（`MerchantMiddleware` → gRPC `GetMerchantAccess`），
**无缓存**。代价是每个商户请求多两次 gRPC 往返；换来的是这几件事在下一次请求就成立，
端到端实测过（同一枚未过期令牌、不重新登录）：

- scope 从 `merchant` 收到 `store` ⇒ 列表当场少到一家，范围外的详情当场 404；
- 改掉门店的 `brand_id` ⇒ 品牌档账号的可见集合立刻跟着变；
- 账号 `status` 改 disabled、商户主体改 suspended ⇒ 下一次请求立刻被拒。

`GetMerchantAccess` 拿不到答案时返回 `Unavailable` → **503**，**不降级成空范围也不降级成
全量**；账号或商户非 active 返回 `PermissionDenied` → 403。**越界与不存在统一 404**
（「这一条我看不到」与「这一条没有」不区分，免得 404/403 的差别泄露别家商户的存在性）。

### 已知缺口（不是遗漏，是这一刀没做）

- **商户端没有令牌续期**：服务端没有 `/v1/merchant/auth/refresh`，也没有 `merchant_sessions`
  表（`MerchantAuthService` 只有 `Login` 与 `Me`）。access token 过期就是重新登录。
- **数据范围没有缓存**：如上，每请求两次 gRPC。
- **商户端订单不能按取杯号筛**：`MerchantOrderQuery` 只认
  `status` / `orderNo` / `source` / `createdFrom` / `createdTo`，客服定位一单只能靠订单号或
  在列表里翻。

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

## 退款闭环

退款跨两个服务，形状是**两段事务夹着一次网络调用**——同一份 `service/create.go` 的三段式
（渠道调用绝不放在 PG 事务里），只是发起方从用户变成了客服。

```
后台审核通过（order-service）
  事务 A   order_after_sales: pending → approved（记 reviewed_by / reviewed_at）
  事务外   gRPC CreateRefund(after_sale_no)          ← 幂等键就是售后单号
  事务 B   order_after_sales: approved → refunding（写 refund_no）
           orders: paid / completed → refunding
     ↓
payment-service：建 payment_refunds(pending) → 渠道 → succeeded / failed / processing
     ↓
payment.refund.succeeded | payment.refund.failed
     ↓
order-service 收口：售后单与订单各自落到结论
```

**停在 `approved` 不是卡住。** 事务 A 与 B 之间挂掉（支付服务抖了、进程重启），售后单就停在
`approved`——那是一句准确的描述：「同意退，退款单还没建成」。后台在那一行上给一个「发起
退款」按钮（`POST /v1/admin/after-sales/{no}/refund`），按的就是重试这条路。重试是安全的：
建退款单的幂等键是售后单号，已经建过的那一次会原样把同一张退款单还回来，不会退两次钱。

为什么重试是独立入口而不是「再点一次通过」：审核是一次**决定**，只该发生一次；发起退款是
那个决定的**执行**，可以重试任意多次。合成一个的话，审核人点第二次会拿到 409（单已经不在
待审核状态），那张单就再没有动作能把它推下去了。

**订单恢复到哪个状态，是读回来的，不是猜的。** 退款失败、或者按行退成功，订单都要回到
「退款前那个状态」——可能 `paid`，也可能 `completed`。这个原状态从
`order_state_transitions` 里最近一条 `to_status='refunding'` 的 `from_status` 读回，不新增
列。一律回 `paid` 的话，一张已经取过杯的单会被降级成「还没做完」，与事实正好相反。

**按行退成功不把整单判死**：判据是售后单的 `scope`，不是金额。只退了一杯的订单还活着，
剩下那行还要履约。

**这条链装不下设备单。** 取货码与线下刷卡机的单**没有 `payments` 行**（钱在机器那边收过了），
而 `payment_refunds.payment_id` 是 `NOT NULL REFERENCES payments(id)`。所以入口在
`ApplyAfterSale` 就收窄：`orders.payment_no` 为空的单直接拒（`ErrOrderHasNoPayment`），
不是「以后再说」，是结构上填不出那个外键。要做得先定设备余额的退款语义，那是设备域的事。

**咖啡豆出资走的是另一条路**：它不经渠道，payment-service 只把那一行
`payment_refund_fundings` 标记成功，真正的冲正在 account-service——它消费
`order.after_sale.refunded`（本服务的退款成功事件经订单域转出来的那一条），在**钱退成
那一刻**把这一单扣过的豆还回去（按 `refundAmount` 算差额，重复申请不会退第二次）。
payment-service **不**调 `ReverseCoffeeBeanEntry`，否则就是同一个动作两个触发源。

时点与福卡的追回是同一拍，理由也一样：豆是支付时就已经收下的钱，还回去就是退款本身，
所以它跟着钱走。**审核通过（`approved`）那一拍什么都不做**——通过了只代表「同意退」，
钱还在渠道那边。挂在「通过」上会多出一个「同意了但钱没出去」的中间态，退款失败时还得把
已经还回去的豆再扣一遍；挂在钱的结果上，每种结局只对应一个动作，退款失败时豆根本不需要
回滚（它从未被动过）。见 `account-service/internal/service/event.go` 里
`handleAfterSaleReviewed` 与 `reverseBeansForRefund` 的注释。

**福卡在不在这条链上，取决于问的是哪一半。** 它只能抽奖、不能出资（支付方式只可能是账户
出资或某条渠道的 code，没有福卡这一档），所以不存在「福卡出资冲正」——**出资**那一半它完全
不在。**赠品**那一半它全程都在：申请退款即冻结（`order.after_sale.applied`），随后由退款结果
收口（`order.after_sale.refunded` / `.refund_failed`，见上面那张事件表）——退成了就把那几笔
发放冲正注销掉，没退成就解冻。冻结行只有这两个终局，不会有「钱退了卡还在」的第三种。

申请之前就已经被抽掉的卡追不回来（冻结时按可用余额钳过），那部分不硬扣：冲正总额以冻结额
为上限，短少的张数写进冻结行的 `reason`（「N 张已参与抽奖，追不回来」），客服在订单详情的
福卡页签上直接看得见。`orders.fortune_card_snapshot` 拆不动时的整单全冻（见
`fortuneCardFreezeKeys`）同样是宁可多冻：多冻是可逆的，少冻才不可逆。

**明确不做**（写在这里，免得下次当成遗漏）：券返还、会员权益恢复、分账回退——前两者要对端的
服务今天没有对应能力，后者方案原文已标「本轮未实现」。**福卡的追回不在这一列**（它是账户域
这一刀做的事，见上）；lottery-service 自己要不要接 `order.after_sale.refunded` 是另一回事，
它那边不接也不影响卡被扣回去。

## 分账与结算的归属

**不建 settlement-service。** 渠道分账、分润规则与结算单都并入 payment-service，它从此是
**资金域**：收钱（发起 / 回调 / 结算订单）、退钱、对账、以及钱怎么分下去，是同一条链上的
四段，不是四个域。契约里那行空壳 `service SettlementService {}` 保持空着——它从来没有过一条
需要固化的内部调用，而「将来会有人调它」不是建一个服务的理由。

分账对 payment-service 是**一条渠道协议调用**（服务商分账是渠道自己的接口），它的输入是
支付单与分法，而这两样都在支付库里。拆出去以后，那个服务要拿支付单就只能：反向读
`panda_payment`（拆库没做完），或者等一条事件再回头补（把「结算单生成」变成一件依赖消息
送达的事）。分润规则与结算单更是同一个事务里的两件事：规则算出数、数被冻结进单子。

老系统 `panda_serve` 有可对照的形态（`internal/models/order_settle.go` 的 `OrderSettleDetail`
与后台结算模块的 `settle_rules` / `rule_items` / `roles` / `accounts` / `details` / `logs`
六张表），**只读对照，不照抄**。它的分账是**随支付请求一次性下发**的：`DivisionFlag` +
`SubOrders` 挂在银联商务全民付的下单报文里（`order_membership_service.go:215`），平台那一份
用**差额倒挤**（主订单金额 − 子订单之和）。V2 这边微信分账是支付成功后单独发起（还要多一步
完结），形态与它不同，所以「照着老系统把分账塞进支付请求」这条路走不通。

另外它的分账子商户号是直接写在订单行上的三列，而 V2 的 `payments` 只有 `order_no` 与
`user_id`——门店与商户的维度得从别处来（下面那条前置契约说的就是这件事）。

三条边界，定这条归属的时候就一起定死：

- **权限码分开，`settlement:*` 绝不并进支付那几枚。** 定这条的时候支付侧还有一枚
  `payment:manage`（能写渠道的凭据槽，也就是能改签名用的密钥）；那枚码已经随渠道表一起删了
  （见上面支付库那一段），今天支付侧只剩 `payment:read`，而 `settlement:read` /
  `settlement:manage` / `settlement:payout` 是**唯一**还能改「钱怎么分」的码。混进去的后果
  同一条：让一个只该看支付单的客服拿到改分账比例的能力。分开的理由与 membership 的三分
  （`read` / `manage` / `adjust`）是同一条。
- **结算记录只存值引用 + 冻结快照。** 比例、主体名、金额这些**在结算那一刻**抄进单子，此后
  不回头现查——否则商户改一次名字、门店换一次归属，历史结算单跟着变，而结算单是要拿去对账
  的。引用的 id（`payment_no` / `store_id` / `merchant_id`）只存值，跨库不建外键（与全仓一致）。
- **依赖单向：payment → merchant / coffee-machine。** 冻结快照要的名字在那两个库里，由
  payment 在结算那一刻经 gRPC 取回；**不许反向**，也不许谁把支付库的单子抄一份过去。
  订单维度的来路见本段末尾（支付服务不读订单库这条不变量不动）。

**券的成本不单独考虑**（2026-09-18 定）：分账基数就是用户实付金额（券后），券的让利按分成
比例在各方之间摊掉，不为「谁发的券、谁该承担」建维度。方案里点过那个缺口（「券的成本无处
归属」），结论是这台账认「按实付分」这个口径——真要按发券方算成本，得让一笔分账有两个基数、
恒等式跟着改，不做。

**那条前置契约已经补上了**（2026-09-18）。当时的问题是：支付库里没有门店/设备维度
（`payments` 没有 store/brand 列，`CreatePaymentRequest` 也没有），而规则按
device/store/brand 命中、任务又在发起支付那一刻就建，所以**契约不补，分账任务建不出来**，
且是安静地建不出来——没有报错，只是永远没有分账发生。

补法是 `CreatePaymentRequest` 追加 `store_id` / `device_id` / `biz_type` 三个字段：
`biz_type` 是命中键的第一段（唯一键 `(biz_type, scope_type, scope_ref)`），由 order-service
从**订单行**推（有饮品 → `coffee`，否则有会员 → `membership`，否则有加购 → `addon_product`，
兜底 `coffee`；混单只压成一个值，一笔支付一条任务）；门店与设备直接取订单上的值引用。
payment-service 拿到后在建支付单的**同一个事务里**命中规则、算金额、写
`settlement_tasks` + `settlement_receivers`——任务与支付单同生共死，规则也在那一刻冻结。

补上之后**仍然命中不了两档**，这是有意留下、写在代码注释里的：**brand**（订单库里没有品牌，
`brands/stores` 在商户域，要命中得先从商户域经 gRPC 取「门店 → 品牌」）、**product**（一笔
支付可能含多个商品，而这里是「一笔支付一条任务」，没有单一商品可指）。命中函数的签名收全
五档，今天的调用方只给得出 device / store 两档——**写全但暂时命不中**。

另外两处刻意的不做：**账户出资（咖啡豆）不建分账任务**（渠道分账分的是渠道里的钱，豆支付
没有渠道资金可动），**查规则失败让这次发起直接失败**（不做老系统「查失败就当没规则、全归
平台」的静默兜底——那是把一次配置故障变成一次错误的分账）。

分账任务**在发起支付时就建好**，支付成功之后只剩推进状态——发起支付的请求里正好带着订单、
门店、设备与实付金额（都由 order 权威给出），那一刻命中规则、算好各家金额最省事；等成功了
再回头凑维度，要么跨服务反查，要么补一次契约。规则因此在**发起支付那一刻冻结**。代价是
`pending` 里混着还没付款的任务，所以**扫「待发起」必须连 `payments` 一起过滤**（只取
`succeeded`），结算归集**只捞 succeeded 的明细**；状态机也为此多一个终态 `cancelled`
（支付单进 failed / expired / closed 时写，任务与接收方一起，只能从 `pending` 进）。
这几条的理由都写在 008 的文件头。

四件事里**只落了分账的「建任务」那一刀**（2026-09-18）：发起支付时命中规则、按
percent / fixed / remainder 算出各家金额、在同一个事务里写 `settlement_tasks` +
`settlement_receivers`；支付单进 failed / expired 时，还停在 pending 的任务与接收方一起转
`cancelled`（三个写入点：建单即失败、回调判失败、超时关单）。金额恒等式
`base_amount = platform_amount + Σ amount` 跨表、CHECK 表达不了，由写入前核一次，对账再核。

**第二刀（2026-09-22，分账后台）**补的是三样：① 配置面——分账规则（含接收方明细）与分账账户
的后台 CRUD，此前只能手工写 SQL；② 查询面——分账任务与接收方的只读列表与详情；③ **把分账真的
发出去**——发起支付时把算好的子单拼成下单报文里的 `divisionFlag` / `platformAmount` /
`subOrders`，支付成功回调里把任务与接收方从 `pending` 置成 `succeeded`。第 ③ 条前，本地建了账
却从没告诉渠道，任务只会越攒越多；而且**银联商务是随支付一次下发**（老系统就是这条路），
不是微信那套四步，所以「发起」这个动作根本不存在——它在报文的字段里。

**还没做**的是：冲正（退款时不回退分账）、结算单（`settlement_statements` 两张表）与打款、
异步分账与其子单确认、查分账，以及退款与对账。订单维度的走法见上——**不是 payment 反向拉**，
它今天不读订单库，这条不变量不为了结算开口子。

## 明确不做的

- **不建 settlement-service**：分账、分润规则与结算单并入 payment-service 的资金域，
  老系统那套服务商分账只读对照、不照抄（见「分账与结算的归属」）。
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
  已经在 `panda_payment` 里建好，但没有代码也没有 worker——它们都要真实渠道凭据，没有
  凭据时写出来只能验参数构造，那不是验证，是猜。
- **适配器只有一个：`ums`（银联商务全民付）**。收钱今天只有四条路——咖啡豆（账户出资）、
  银联商务小程序支付、银联商务 H5、取货码——只有后两条里的第三方那一段要跟别人说话。
  早先这里注册过五个协议族（`form_md5` / `hmac_body` / `ums` / `wechat_v3` / `manual`），
  对应丰选万联 / 优联 / 首创 / 北方工业 / 家园消费金五家与微信直连：那些渠道**一家都没接**，
  先写着的代码只能验参数构造，那不是验证是猜，所以删了（真要用时从 git 历史取回；新增一族的
  做法见上面支付库那一段）。**`ums` 的回调验签没有老系统可对照**：老系统的微信走银联商务，
  而它自己的银联回调**根本没验签**（`ParseNotify` 是个只做 `json.Unmarshal` 的空壳），所以
  V2 这套是按出站签名对称推断出来的。上线前必须用真实凭据实测回调，确认它到底带不带签名头、
  带的是不是同一个算法。「验签失败不改支付状态」与「重放被唯一键挡住」两条行为有单测钉着
  （`service` 的回调路径 + `ums` 的验签用例），但那是**对着上面这个假设**验的。
- **`ums` 一族下面挂着两种协议**（分派点是 `catalog.Method.Action`，不是渠道）。小程序那条是
  `POST + JSON 体 + OPEN-BODY-SIG 请求头`；**H5 那条是 `GET + 参数全拼 URL + OPEN-FORM-PARAM`，
  而且没有应答报文——渠道回 302 让浏览器去收银台**，「3xx + `Location`」就是「单建成了」的
  唯一证据。四条 H5 下单路径（支付宝 / 微信 / 云闪付 / 微信转小程序）写在 `catalog` 的常量表里
  （每条方式各自的 `createPath`），它们没有合理的默认值：随手挑一条当默认，会把一笔支付宝的
  支付打到微信的接口上。入站那套签名（回调与结果页回跳）是**第三套**——
  参数排序拼串、末尾接通讯密钥、取 MD5 或 SHA256，与出站那两套方向相反、算法也不同，
  所以这一族要**两把凭据**（appKey 出站、通讯密钥入站），都走既有的槽机制，
  解不到就 fail closed。协议要点与实测结论见 `docs/unionpay-h5-pay.md`。
  账户出资（咖啡豆走
  `action=account`）**已落地一条纯豆全额的路**：豆账户在 account-service 里（§5.6 的
  另一半，余额 + 只追加流水、单位分、永不过期、不冻结），后台人工调整是它唯一的来路
  （`POST /v1/admin/coffee-beans/{userId}/adjustments`，`account:manage`），发起支付时同步
  调用扣减、没有 `pending` 中间态（钱在自家库里，扣成功就是成功）。**这一条路目前只支持
  「全额用豆付」**：混合出资（豆 + 渠道凑一单）没做；payment-service 的退款单也没做——纯豆
  单的退款由 account-service 自己在退款成功时冲正，见上面「售后与退款」那一段。**支付方式与渠道
  今天不是数据，是代码里的常量表**（`internal/catalog`）：后台那一页连同六个写接口、
  `payment:manage` 那枚码、两张表和 `deploy/dev-seed/` 的种子一起删了（payment/009、
  identity/032），因为原来那套要求运营填二三十个协议键——「要不要开咖啡豆支付」今天就是
  catalog 里的那一行，不是点一下后台。
  福卡不是一种支付方式——它是下单赠送的抽奖凭证，只能抽奖、不能出资（order/003、payment/004 已收窄）。
- **福卡到「发放 + 退款冻结 + 追回」为止**：余额、不可变流水、发放/扣减/冲正、订单完成
  自动入账，退款申请期间的冻结，以及三条收口——驳回/撤销 ⇒ 解冻、退款失败 ⇒ 解冻、
  **退款成功 ⇒ 追回**（解冻 + 冲正那几笔发放，见上面「售后与退款」那一段）。追不回来的
  那几张不硬扣：冻结时就按可用余额钳过，短少的张数写进冻结行的 `reason`。**豆那一半也在
  同一拍上回来**（`order.after_sale.refunded` 一起做：先追卡、再还豆，见上面「售后与退款」
  那一段），审核通过那一拍两边都不动——福卡是还没花出去的凭证，豆是支付时就已经收下的钱，
  两者都要等钱真退了再回。退款失败时它们也不需要任何回滚：卡由 `refund_failed` 解冻，
  豆从未被动过。渠道支付的订单走到豆的冲正这条路上会什么都不发生（找不到那笔扣减），那
  是常态不是异常。服务端**不计算**承诺福卡——`fortuneCardsExpected` 仍采信
  调用方，与「价格来自调用方」是同一个已知缺口；加购加赠活动的规则归属也仍未定，本轮只是
  把它的快照原样记进流水。人工冻结/解冻没有后台入口（冻结只有「售后申请」一个触发源），
  也没有补冻的定时兜底——事件丢了就是丢了，与发放同一条取舍。`miniapp/` 前端仍是空目录
  （余额与冻结字段已经在接口里回来了，页面下一轮）。

- **抽奖到「开奖 + 中奖记录」为止**（lottery-service）：门店开通、活动与奖池、期次滚动、
  用福卡参与、自动/人工开奖、中奖记录都落库了，`DeductFortuneCards` 的调用方就是它。
  `available = balance - frozen_balance` 是它判「够不够扣」的判据，也是冻结唯一的判据改动。
  **没做的**：核销 / 领取 / 换奖（中奖一律停在 `pending`，列与状态机按最终形态建全了）、
  抽奖域的商户端入口（`/v1/merchant/lottery/*` 一条都没有；商户端已有的门店/设备/订单三屏
  见上面「商户端入口与数据范围」）、退款成功时对**已参与期次**的冲正（卡本身已经由账户域
  追回了，这里说的是把那些参与记录退掉；抽奖域**不消费任何
  事件**——发券那条线的自动发放要 coupon-service 的 gRPC，而那是空壳）、重抽、开奖审批流。
  规则先记在这儿：**已开奖的期次是终局；未开奖期次内的参与在退款成功时冲正 + 回退
  `participant_count`**（跌破门槛则把期次开回 `open`）。
- **支付方式列表接口不做**：「这台设备能用哪几条方式」是设备域 `device_payment_methods`
  的事实，归设备域；支付服务不为它开一条从支付库读咖啡机库的口子。
- **商户端没有支付页面**：商户不查支付单——那是运营后台的事（只读，见上面支付那一段），
  merchant-web 一行都没改。支付方式与渠道今天在代码里，两边都没有可配的入口。

- **订货 / 库存域整个不做了**（2026-09-22）：删掉的不是「一半功能」，是整个域——服务、迁移、
  proto、后台七页与它的权限一起没了（见上面「订货 / 库存域已从 V2 删掉」）。它先前做到
  「入库闭环为止」（建物料与配方、仓库与批次、开入库单 → 确认 → 作废另写反向流水），
  没做的那些（出库 / 调拨 / 调整单 / 订货单 / 出杯扣料）也不再有归属。将来单独拆一个服务
  重做时，那一段的判断与教训仍然成立，别当新问题重新踩一遍。
