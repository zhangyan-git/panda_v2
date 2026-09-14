# coffee-machine-service

设备域服务（方案 §5.14）：设备厂商及其接入凭据、咖啡机设备与支付方式、饮品与供应
关系、设备事件与余额流水。数据库是 `panda_coffee_machine`，迁移集是
`migrations/coffee_machine`。

## 边界

出杯任务、出杯回调、出杯重试与设备对账**不在这里**，在 fulfillment-service
（§5.11 L496-499、§5.14 L547）。本服务对出杯链路的全部义务是**被读**：下单时校验
设备状态（§5.8 L454），`GetDevice` 就是那条路。

两个容易搞错的地方，写在这里免得后来人再问一遍：

- **`status` 与 `vendorOnline` 不是一件事。** `status`（active/disabled）是「本系统
  让不让这台设备接单」，`vendorOnline` 是「厂商说它此刻通不通」。后者是 nullable：
  null 表示从未同步过，**不等于离线**。下单校验两条都要看，调用方按 null 分出第三种
  情况。
- **本库不持有 `merchant_id`。** 设备通过 `store_id` 关联部署点位，商户归属从点位那
  一侧推。`store_id`、`payment_method_id` 这类外部 ID 都只是值引用，本服务不校验对方
  是否存在，也不建跨库外键。

## 厂商账号与令牌

接入凭据与令牌由**本服务唯一持有并签发**（`manufacturer_credentials` 是唯一权威副本）。
fulfillment-service 直连厂商执行出杯，调用前向本服务取一组只读的调用材料：
`access_token` / `token_expires_at` / `api_base_url` / `signing_secret`（出杯回调的
验签在 fulfillment 侧做，所以签名密钥也要给）。两个适配器各自脱敏、各自记 Provider
调用流水，只有账号与令牌收敛到一个签发点。

实现该契约时的三个要点：刷新令牌对那一行加行锁（保证只有一个副本去刷）、留提前刷新
窗口、刷新失败**保留旧令牌**而不是清空——清空会让 fulfillment 手里的缓存一起作废，
把一次厂商抖动放大成出杯全停。

## 现状

已落地：迁移、健康检查（`/livez`、`/readyz`）、后台主数据读接口、内部只读 RPC
`GetDevice`，以及后台的设备域写路径——厂商／设备／饮品的新增、修改与启停，设备余额调整。

写接口的权限码有三个：`coffee_machine:read`、`coffee_machine:manage`、
`coffee_machine:balance`。后两个由身份库迁移 `identity/014_coffee_machine_write.sql` 建出并绑定
`super_admin`；新环境要跑过这条迁移，否则写接口会回 401。挂路由时**一条路径只注册一次**
（见 `internal/routes/admin.go`）——底层 gorilla/mux 的 `HandleFunc` 是追加而非按路径合并，
对同一路径注册两次，先注册的那条会吃掉所有方法。

余额调整属于方案 §11.6 L893 必审清单，审计在同一个业务事务里往本库 `message_outbox` 追加
一条 `admin.operation.logged`（用 `platform/audit.NewRecorder()`），**不要**新建本地审计表——
审计落在身份库的 `admin_operation_logs`。`device_balance_ledger` 上有 BEFORE DELETE/UPDATE
触发器，流水只增不改不删；这也意味着有流水的设备在库里删不掉，写集成测试夹具时要留意。

尚未落地：删除接口（三张主数据都没有 DELETE）、设备与饮品的关系编辑（只读 `device_drinks`）、
设备与饮品同步、厂商状态拉取。

## 本地运行

```sh
DB_MIGRATE_ON_START=true \
COFFEE_MACHINE_DATABASE_URL='postgres://panda:replace-me@localhost:5432/panda_coffee_machine?sslmode=disable' \
USER_GRPC_ADDR=127.0.0.1:19093 \
MERCHANT_INTERNAL_TOKEN='<32 字节以上>' \
JWT_SECRET='<32 字节以上>' JWT_ISSUER=panda PANDA_ENV=dev \
go run ./cmd/api
```

`USER_GRPC_ADDR` 是必需的：后台每条路由都要按请求去 user-service 取实时授权。
`MERCHANT_INTERNAL_TOKEN` 在本仓库里是服务之间互认的那一枚共享令牌（名字带 merchant
是历史），gRPC 用它校验内部调用方。

集成测试需要一个可连的库，不设就跳过：

```sh
COFFEE_MACHINE_DATABASE_URL='postgres://panda:replace-me@localhost:5432/panda_coffee_machine?sslmode=disable' go test ./...
```
