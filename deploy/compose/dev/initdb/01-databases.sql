-- 数据库拆分后的第二个库。
--
-- POSTGRES_DB 已经建出身份库（panda_identity），这里补建其余各服务自有的库。
-- 本目录只在数据卷首次初始化时执行一次：既有的 dev 卷不会重跑，
-- 那种情况下手工执行一次 CREATE DATABASE panda_merchant / panda_coupon /
-- panda_coffee_machine / panda_order，再用
-- `panda-migrate -database "$MERCHANT_DATABASE_URL" apply merchant`
-- （或把库名与集合名换成 coupon / coffee_machine / order）建表。

CREATE DATABASE panda_merchant;
CREATE DATABASE panda_coupon;
CREATE DATABASE panda_coffee_machine;
CREATE DATABASE panda_order;
