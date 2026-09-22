-- payment/009：支付方式与渠道不再是数据
--
-- 这一条把 001 建的两张表删掉：payment_channels（一个渠道 = 一套对接配置）与 payment_methods
-- （用户能选哪几种）。它们的前提已经不成立——支付方式与渠道今天是**代码里的常量表**
-- （见 payment-service 的 internal/catalog）：四种方式、六个 code、一条银联商务渠道。
--
-- 为什么可以删（不是「暂时不用」）：
--   1. 渠道配置的用处是让运营去填 15–30 个协议键（密钥名、签名字段大小写、成功码、时间戳
--      格式），而今天只剩银联商务一条渠道、两条协议，那些键写死在适配器里（见 provider/ums）。
--   2. 凭据不进库了。007 加的 payment_channels.secrets 是密文槽，解密要一把主密钥；凭据改成
--      按名字读环境变量之后，没有密文要解，连 PAYMENT_SECRET_KEY 都不再是一个启动条件。
--   3. 那两张表的每一列在别处都有落点：payments 记的是「这一单走的哪条渠道、哪种方式」，
--      而渠道名与方式 code 本身就够了（从前要 JOIN 出来）。
--
-- 与 001 的关系：001 写下的是一套「接第三方渠道」的形状，本文件把它换成「四种方式写死在代码
-- 里」。001 已应用过，按「已应用的迁移不改」的惯例不动它，以本文件为准。
--
-- # 回填是有损还是无损：无损
--
-- 指渠道的列每一处都有落点，所以换列不丢信息（下面第 1–4 段逐一处理）：
--   - payments.channel_id          → payments.provider         （渠道名）
--   - payments.payment_method_id   → payments.payment_method   （方式 code）
--   - payment_provider_calls / payment_notifications 本来就有 provider 列（它们记的是「这次
--     调用/这条通知属于哪条渠道」），channel_id 是同一件事的第二份写法，删掉。
--   - payment_transactions 只有 channel_id，要**先加 provider 再回填**（见第 2 段）。
--   - settlement_accounts.channel_id        → …provider（渠道名）
--   - payment_agreements.channel_id / payment_method_id → …provider / payment_method
--   - reconciliation_batches.channel_id     → 它自己的 provider 列（本来就有）
--
-- 后两张（002 建的代扣签约与对账批次）今天都是空表、没有代码在读写：代扣与对账本轮没做。
-- 它们照样要处理到位，不拆外键就删不掉 payment_channels。
--
-- payments 上那两列**可空语义、但落成 NOT NULL DEFAULT ''**：账户出资（纯咖啡豆）没有渠道，
-- 它从前是 NULL，今天用一个空串表示同一件事——空串在 TEXT 上能直接比较，不需要每个读它的地方
-- 都写一遍 NULLIF/COALESCE（这也是 create.go 与仓储里那几处 NULLIF(...)::uuid 消失的原因）。
--
-- 老库迁移过来的行会不会漏：不会。回填是**按 id 关联取名字**，取不到的行（理论上不存在，
-- 外键挡着）会留在空串上——那是「没有渠道」，与账户出资同形，而 payments.provider 为空时
-- 回调那条路会拒绝（见 repository.notificationChannelMatches）。
--
-- # 空库跑这条的结果必须一样
--
-- 生产建新库时这两张表里有种子行、payments 是空的：回填 0 行，DROP 照常。两边结果一致。
--
-- # 没有 Down
--
-- 与 drinks 那条迁移同一个口径：回滚靠备份，不靠反向迁移把裸 uuid 编出来——那张表删了之后
-- 没有任何东西还记得某个 uuid 对应哪条渠道。
--
-- # 本文件不覆盖的地方
--
-- coffee_machine 库的 device_payment_methods.payment_method_id 是 UUID 列，按值引用
-- payment_methods.id。那张表今天是空的、代码里也没有读写它的地方（设备支持哪几种支付方式
-- 属于设备域，尚未做），所以本文件不动它——跨库的改动要由 coffee_machine 的迁移来做，而它
-- 现在没有可迁的数据。
--
-- **但它欠着一条迁移**：支付方式在今天已经没有 UUID 了（值是 catalog 里的 code），所以那一列
-- 的形状与新的词表对不上——设备域真要做「这台机器支持哪几种支付方式」时，第一件事是把这一列
-- 改成 TEXT 并改存 code，不是往里面写 uuid。写在这里是因为那条迁移不属于本文件、也没人排期。

BEGIN;

-- ============================================================
-- 1) payments：先把两列加上并回填，再拆外键、删旧列
-- ============================================================

ALTER TABLE payments ADD COLUMN provider TEXT NOT NULL DEFAULT '';
ALTER TABLE payments ADD COLUMN payment_method TEXT NOT NULL DEFAULT '';

UPDATE payments p SET provider = c.provider
    FROM payment_channels c WHERE p.channel_id = c.id;
UPDATE payments p SET payment_method = m.code
    FROM payment_methods m WHERE p.payment_method_id = m.id;

ALTER TABLE payments DROP CONSTRAINT payments_channel_id_fkey;
ALTER TABLE payments DROP CONSTRAINT payments_payment_method_id_fkey;
ALTER TABLE payments DROP COLUMN channel_id;
ALTER TABLE payments DROP COLUMN payment_method_id;

COMMENT ON COLUMN payments.provider IS
    '走哪条渠道，值是渠道**在代码里**的名字（payment-service 的 catalog.Channel.Provider，'
    '今天只有 ums）。账户出资（纯咖啡豆）是空串——那一列可空的语义，用空串表达。'
    '这条渠道的事实与回调 URL 里那一段是同名的，回调据此与支付单对一遍'
    '（见 repository.notificationChannelMatches）。'
    '⚠️ 老行上这一列是**照 payment_channels.provider 抄的**，而那一列按 001 的定义是**协议族**'
    '（manual / form_md5 / hmac_body / ums / wechat_v3），不是渠道名。今天有数据的库上它只可能'
    '是 ums——能收到回调的渠道只有 catalog 里那一条，而老库里属于别的族的支付单，回调今天'
    '连渠道都解析不到（/v1/payments/callback/<那一段> 不在 catalog 里）。留着原族名只是给排查'
    '留一条线索，不是一条能用的路由。';
COMMENT ON COLUMN payments.payment_method IS
    '用户是在哪一档支付方式下发起的，值是 catalog 里的 code（如 ums_miniapp_wechat / '
    'coffee_bean），不是某个 UUID。它从前是 payment_methods.id，那张表已随本文件删除。'
    '展示与统计用它；发起那次已经是按 code 分派的，不回头读这一列。';

-- ============================================================
-- 2) 三张流水表：渠道那一列一律换成渠道名
-- ============================================================
--
-- 三张表都装「这一行属于哪条渠道」，但它们的现状不同，不能一个写法套三遍：
--
--   - payment_provider_calls / payment_notifications **本来就有** provider 列（写调用记录与
--     写通知那一刻就知道渠道名），channel_id 是同一件事的第二份写法：只补空、不覆盖。
--   - payment_transactions **只有 channel_id**（001 建的），没有 provider。所以它和 payments
--     一样要先加列再回填——少写这一步的代价是**写流水的那条路直接报错**
--     （column "provider" ... does not exist），而它挂在支付成功那条路上：钱收了、流水写不进去。
--
-- 「payment_transactions.channel_id 没有外键」（001 的注释写着理由：流水是对账基准，渠道配置行
-- 将来怎样都不该让已发生的流水对不上）——没有外键不改变这一列装着什么，它照样要在删之前
-- 换成名字。
UPDATE payment_provider_calls pc SET provider = c.provider
    FROM payment_channels c WHERE pc.channel_id = c.id AND pc.provider = '';
UPDATE payment_notifications n SET provider = c.provider
    FROM payment_channels c WHERE n.channel_id = c.id AND n.provider = '';

-- 先加列（与第 1 段 payments 那两行同一个形状）：上面那句注释说的是「要先加列再回填」，
-- 而**先加列这一步是不能省的**——省了的话这条迁移在空库上跑得干干净净，库却少一列，
-- 直到写流水那条路报 column "provider" does not exist 才被发现，而那时钱已经收了。
ALTER TABLE payment_transactions ADD COLUMN provider TEXT NOT NULL DEFAULT '';

-- 流水是**只追加**的：001 的 payment_transactions_append_only 触发器无条件拒绝 UPDATE（与
-- stock_movements 那条同一个形状）。回填正好是一次 UPDATE，不摘掉它这一步必然失败——**在一个
-- 有历史的库上整个迁移都会回滚**，而它跑得通的样子正是最该警惕的：只有空库才不报错，
-- 也就是说这条迁移会在生产上第一次真正被用到的那天炸掉。
--
-- 摘下来的是「禁止改这一列」这一条保护，位置就在这两行之间，装回去是下一句。语句本身只改
-- 本次新增的那一列（channel_id → provider 的同一次换名），不碰金额、方向、单号。
ALTER TABLE payment_transactions DISABLE TRIGGER payment_transactions_append_only;
UPDATE payment_transactions t SET provider = c.provider
    FROM payment_channels c WHERE t.channel_id = c.id;
ALTER TABLE payment_transactions ENABLE TRIGGER payment_transactions_append_only;

ALTER TABLE payment_provider_calls DROP CONSTRAINT payment_provider_calls_channel_id_fkey;
ALTER TABLE payment_notifications DROP CONSTRAINT payment_notifications_channel_id_fkey;

ALTER TABLE payment_provider_calls DROP COLUMN channel_id;
ALTER TABLE payment_notifications DROP COLUMN channel_id;
ALTER TABLE payment_transactions DROP COLUMN channel_id;

COMMENT ON COLUMN payment_transactions.provider IS
    '这一行流水属于哪条渠道（渠道名，与 payments.provider 同词表）。它从前是 channel_id '
    '（指向 payment_channels 的 uuid），那一列随那张表一起换成了名字。';

-- ============================================================
-- 3) 002 那两张空表：代扣签约与对账批次
-- ============================================================
--
-- 它们同样按 uuid 指渠道，不拆外键就删不掉 payment_channels。两张表今天**都是空的、也没有
-- 任何代码读写**（代扣与对账本轮没做，见 002 的文件头），所以这里只做形状对齐，不回填数据。
--
-- 加工与别处一致：里面装的是渠道名与方式 code，所以列名跟着值走（channel_id → provider、
-- payment_method_id → payment_method）。留着叫 _id 而装名字，下一个人会以为它是一个可以
-- JOIN 回去的键。

ALTER TABLE payment_agreements ADD COLUMN provider TEXT NOT NULL DEFAULT '';
ALTER TABLE payment_agreements ADD COLUMN payment_method TEXT NOT NULL DEFAULT '';

UPDATE payment_agreements a SET provider = c.provider
    FROM payment_channels c WHERE a.channel_id = c.id;
UPDATE payment_agreements a SET payment_method = m.code
    FROM payment_methods m WHERE a.payment_method_id = m.id;

ALTER TABLE payment_agreements DROP CONSTRAINT payment_agreements_channel_id_fkey;
ALTER TABLE payment_agreements DROP CONSTRAINT payment_agreements_payment_method_id_fkey;
ALTER TABLE payment_agreements DROP COLUMN channel_id;
ALTER TABLE payment_agreements DROP COLUMN payment_method_id;

COMMENT ON COLUMN payment_agreements.provider IS
    '走哪条渠道（渠道名）。代扣只能走渠道（微信委托代扣、银联无感支付），所以它非空。';
COMMENT ON COLUMN payment_agreements.payment_method IS
    '签约时用户选的支付方式 code（catalog 里的常量），不是某个 UUID；渠道侧的签约可以没有。';

-- 对账批次：provider 列本来就有，与 channel_id 是同一件事的两份写法。这次收口到 provider 一份，
-- 连带把「同一渠道同一天同一种账单只有一个批次」那条唯一性挪过去——唯一约束和索引都含旧列，
-- 删列会把它们一起带走，所以手工重建（漏掉的话同一个渠道同一天能建出两个批次，
-- 而对账的结论会分在两个批次里，谁都看不全）。
--
-- provider 这一列**本来就是 NOT NULL**（002 建表时就写了，不是这里加的）：建对账批次必须
-- 指名渠道。009 对它做的只有两件事——补空值（下面那句 WHERE b.provider = ''，给「从前手工
-- 塞过空串」的库留的兜底），以及把 002 那个 DEFAULT '' 去掉（见下面的 DROP DEFAULT）。
-- 表空不空都不影响这两句。
UPDATE reconciliation_batches b SET provider = c.provider
    FROM payment_channels c WHERE b.channel_id = c.id AND b.provider = '';

ALTER TABLE reconciliation_batches DROP CONSTRAINT reconciliation_batches_channel_id_fkey;
ALTER TABLE reconciliation_batches DROP CONSTRAINT reconciliation_batches_channel_id_biz_date_bill_type_key;
DROP INDEX reconciliation_batches_channel_idx;
ALTER TABLE reconciliation_batches DROP COLUMN channel_id;

ALTER TABLE reconciliation_batches ALTER COLUMN provider DROP DEFAULT;
ALTER TABLE reconciliation_batches ADD CONSTRAINT reconciliation_batches_provider_biz_date_bill_type_key
    UNIQUE (provider, biz_date, bill_type);
CREATE INDEX reconciliation_batches_provider_idx
    ON reconciliation_batches (provider, biz_date DESC);

-- ============================================================
-- 4) settlement_accounts.channel_id：uuid → 渠道名
-- ============================================================

-- 名字从 channel_id 改成 provider，与 payments 那一列对齐：里面装的是渠道名，不是 id。
--
-- 这一列**不能只改名**（ALTER … RENAME COLUMN 会把两条唯一索引一起带走，那是最省事的写法），
-- 因为类型要从 uuid 变成 text，改名改不了类型。而删列再重加会**连带删掉那两条唯一索引**
-- （它们都含这一列，PostgreSQL 删列时会把依赖它的索引一起丢掉），所以下面手工重建。
--
-- 那两条唯一索引不是装饰：「一个主体在一个渠道上只能有一条启用中的账户」与
-- 「一个渠道下的接收方号只能登记一次」全靠它们，漏建一条之后重复登记会静默成功，
-- 而后果是分账把钱打给同一个接收方两次。
ALTER TABLE settlement_accounts ADD COLUMN provider TEXT;
UPDATE settlement_accounts a SET provider = c.provider
    FROM payment_channels c WHERE a.channel_id = c.id;
-- NOT NULL 保留：分账是渠道能力，没有渠道就没有可用的接收方（008 的列注释）。回填取不到名字
-- 的那些行（外键挡着，理论上不存在）会让下面这一步**失败并回滚整个迁移**——那正是想要的：
-- 一句「这里有行没有渠道名」比一个静默的空串好查。
ALTER TABLE settlement_accounts ALTER COLUMN provider SET NOT NULL;

DROP INDEX settlement_accounts_party_uniq;
DROP INDEX settlement_accounts_receiver_uniq;

ALTER TABLE settlement_accounts DROP CONSTRAINT settlement_accounts_channel_id_fkey;
ALTER TABLE settlement_accounts DROP COLUMN channel_id;

CREATE UNIQUE INDEX settlement_accounts_party_uniq
    ON settlement_accounts (party_type, merchant_ref, brand_ref, store_ref, provider)
    WHERE status = 'enabled';
CREATE UNIQUE INDEX settlement_accounts_receiver_uniq
    ON settlement_accounts (provider, receiver_type, receiver_id);

COMMENT ON COLUMN settlement_accounts.provider IS
    '走哪条渠道（渠道名，与 payments.provider 同词表）。分账是渠道能力：没有渠道就没有可用的'
    '接收方，所以这一列非空。它从前是 settlement_accounts.channel_id（指向 payment_channels '
    '的 uuid），那一列随那张表一起没了。';

-- ============================================================
-- 5) 删表
-- ============================================================
--
-- 到这一步已经没有列指着它们了（外键全部拆完）。legacy_id 的两条唯一索引随表消失——那是
-- 老库 ObjectID 的对照键，只对迁移过来的行有意义，而那批行已经被上面几段回填进新的文本列。

DROP TABLE payment_methods;
DROP TABLE payment_channels;

COMMIT;
