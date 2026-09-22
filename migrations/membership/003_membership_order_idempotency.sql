-- membership/003：把「一单只开一次会员」从 (order_id, change_type) 收紧到 (order_id)
--
-- 001 上那条唯一索引建在 (order_id, change_type) 上，它挡的是「同一条回调把同一种变更记两遍」。
-- 但重投走的**不是同一种变更**：一条 order.paid 第一次投递时这个人还不是会员，走开通分支记
-- 一条 activate；同一条消息再投一次时，memberships 上已经有那一行了，于是走续期分支记一条
-- renew。change_type 不一样，唯一索引拦不住，第二次投递把有效期又叠了一段——一个只付了一次钱的
-- 人拿到了两期会员，而且这件事在流水上看起来完全正常（两条合法的变更记录）。
--
-- 收紧之后的判据是「这个订单有没有产生过开通或续期」，与走的是哪一支无关。refund_adjust 这类
-- 挂在同一张订单上的其它变更不受影响：它们不是购买行为，一条订单可以有多条。
--
-- 这一条是**不变式**，所以钉在库上而不是写在服务层：靠服务层自觉拦截的话，下一个新增的
-- 购买入口（改价重推、补单、人工补录）就得自己记得查一遍，而漏掉的那一次没有任何东西会报错。
DROP INDEX IF EXISTS membership_changes_order_unique;

CREATE UNIQUE INDEX membership_changes_order_unique
    ON membership_changes (order_id)
    WHERE order_id IS NOT NULL AND change_type IN ('activate', 'renew');

COMMENT ON INDEX membership_changes_order_unique IS
    '一个订单只能开通或续期一次；支付成功回调重放不会把会员续两次';
