-- 数据库拆分后的第二个库。
--
-- POSTGRES_DB 已经建出身份库（panda_identity），这里补建其余各服务自有的库。
-- 本目录只在数据卷首次初始化时执行一次：既有的 dev 卷不会重跑，
-- 那种情况下手工执行一次 CREATE DATABASE panda_merchant / panda_coupon /
-- panda_coffee_machine / panda_order / panda_payment / panda_account / panda_lottery，再用
-- `panda-migrate -database "$MERCHANT_DATABASE_URL" apply merchant`
-- （或把库名与集合名换成 coupon / coffee_machine / order / payment / account / lottery）建表。

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
