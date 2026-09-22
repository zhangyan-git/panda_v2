-- payment/006：后台支付单列表的排序索引
--
-- 补一条 payments (created_at DESC, id DESC)。
--
-- # 为什么现在才补
--
-- 001 建索引时列了三条 payments 上的索引，其中 order_idx / user_idx 的注释写的是
-- 「订单/用户视角的查询：后台列表、客服按单号查」。那句话对**带筛选**的查询是成立的，
-- 但后台支付单列表的默认视图是**不带任何筛选**的「最近发生了什么」——它按 created_at
-- 倒序取第一页，而三条索引里没有一条的前导列是 created_at（order_idx 与 user_idx 的
-- 前导列分别是 order_no 与 user_id，pending_expiry_idx 是 status 且带部分条件）。
-- 结果是这条查询走全表扫描 + Top-N 排序。
--
-- 001 之所以漏掉它，是因为**当时没有这条查询**：后台支付页面是本轮才做的
-- （identity/026 的菜单 + payment-service 的后台只读树）。索引跟着查询走，不提前建。
--
-- # status 筛选仍然不索引
--
-- 列表上还有一个按 status 的筛选，它走的是「先筛后排序」，没有配套的
-- (status, created_at DESC)。这是**有意不建**，不是漏了：status 只有 6 个取值、
-- 分布又高度倾斜（绝大多数是 succeeded），这种列上的二级索引对选择性几乎没有帮助，
-- 而 payments 是只增表，多一个索引就是给每一笔支付的成功路径多付一次写放大。
-- 哪天真慢了，先看 created_at 这条能不能撑住，再谈要不要建部分索引。
--
-- # 上生产的注意
--
-- 迁移在事务里跑（见 platform/database/migrate），所以**用不了 CREATE INDEX CONCURRENTLY**，
-- 这条语句在存量数据上会**短暂持写锁**。本仓库的支付库还很小，dev/prod 都是秒级；但真到
-- 几百万行那天，这条要挑低峰窗口上，或者由 DBA 手工 CONCURRENTLY 建好再把它 adopt 过去
-- （`panda-migrate adopt payment 006`）。

BEGIN;

-- 带 id 是为了同一毫秒的两行也有稳定顺序：不含它的分页会在边界上重复或跳过行，
-- 而支付单的 created_at 精度是微秒，同批写入撞在一起是常态不是巧合。
CREATE INDEX payments_admin_list_idx ON payments (created_at DESC, id DESC);

COMMIT;
