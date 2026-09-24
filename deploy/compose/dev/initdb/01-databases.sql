-- 数据库拆分后的第二个库。
--
-- POSTGRES_DB 已经建出身份库（panda_identity），这里补建其余各服务自有的库。
-- 本目录只在数据卷首次初始化时执行一次：既有的 dev 卷不会重跑，
-- 那种情况下手工执行一次 CREATE DATABASE panda_merchant / panda_coupon /
-- panda_coffee_machine / panda_order / panda_payment / panda_account / panda_lottery /
-- panda_membership / panda_partner，再用
-- `panda-migrate -database "$MERCHANT_DATABASE_URL" apply merchant`
-- （或把库名与集合名换成 coupon / coffee_machine / order / payment / account / lottery /
-- membership / partner）建表。

CREATE DATABASE panda_merchant;
CREATE DATABASE panda_coupon;
CREATE DATABASE panda_coffee_machine;
CREATE DATABASE panda_order;
CREATE DATABASE panda_payment;
-- 资产账户域（福卡）。它是最后一个加进来的服务，所以既有的 dev 卷里没有这个库——
-- 那种情况下要手工 createdb，再 `panda-migrate ... apply account`。
CREATE DATABASE panda_account;
-- 抽奖域。同 account 的理由：既有的 dev 卷里也没有这个库，手工 createdb 之后
-- `panda-migrate ... apply lottery`。
CREATE DATABASE panda_lottery;
-- 会员域。同 account 的理由：既有的 dev 卷里也没有这个库，手工 createdb 之后
-- `panda-migrate ... apply membership`。
CREATE DATABASE panda_membership;
-- 开放平台（合作方接入）域。同 account 的理由：既有的 dev 卷里也没有这个库，手工
-- createdb 之后 `panda-migrate ... apply partner`。
CREATE DATABASE panda_partner;

-- 连接默认时区设成东八区，让**看库的人**直接读到北京时间。
--
-- timestamptz 存的是绝对时刻，改这个**不动物任何数据、也不动任何查询结果**——全仓
-- 没有 date_trunc / ::date / AT TIME ZONE 这类依赖会话时区的写法。它只决定 psql 与
-- GUI 客户端把时间渲染成什么：不设就是 UTC，`2026-09-23 09:46:05+00` 其实就是北京时间
-- 17:46:05，核对订单时间时要先在脑子里加 8 小时。
--
-- 用 ALTER ROLE 而不是逐库 ALTER DATABASE：一条覆盖全部库，将来新增的库也自动生效。
-- 角色名与上面那些库名一样是写死的（这个集群里只有 panda 一个角色，连接一律用它）。
-- 本目录同样只在首次初始化时执行一次，既有的 dev 卷要手工执行一次这一条。
ALTER ROLE panda SET timezone = 'Asia/Shanghai';
