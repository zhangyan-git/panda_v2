-- payment/005：账户出资（纯咖啡豆）的扣减留痕
--
-- 纯豆那条路上，钱是**分两步**离开我们的，两步各自提交：
--
--   1. 在账户域扣豆（跨服务 RPC，账户域那边一次提交）
--   2. 在本地把支付单推到 succeeded（另一个事务，见 SettleAccountPayment）
--
-- 两步之间没有任何东西连着：进程被 kill、库抖动、或者这张单在这中间被超时关单收走，豆就已经
-- 真的扣走了，而本地**一个字段都没留下**——payment_fundings 那一行在没提交的那个事务里，
-- payments 上也没有任何列指向账户域那笔账变。事后只能靠人拿着订单号去账户库翻
-- coffee_bean_entries，可翻哪一张单、翻哪一笔，没有任何线索。
--
-- 现在那两条兜底各自补了一半：发起方拿同一个 request_id 重发会在账户域命中幂等键、回放原账变
-- （见 client.DeductRequest），但那只在「用户还会重试」时成立；一旦这张单到了超时点，它会被
-- 关成 expired，连重试都再也结不了。
--
-- 所以：
--   account_entry_id   扣豆返回的**那一刻**就落库（RecordAccountDeduction），先于结算。
--   account_funded_at  豆真正动了的时刻（账户域那笔账变的发生时刻）。
--
-- 有值就表示「这笔钱确实动过」。这一条判据有两个用途，缺一不可：
--
--   * 超时关单不再碰它们（ExpireOverduePayments 的扫描条件排除了 account_entry_id 非空的
--     行）。关单会把 payment_fundings 里 reserved 的行标成 released——那是对「钱从来没动过」
--     的描述，而这里的钱已经动了，把它标成 released 就是一次记账上的撒谎。
--   * 补偿任务把它们结算掉（SettleOverdueAccountPayments）。豆已经扣了，这张单就必须成，
--     而不是等着一个人来发现。
--
-- 为什么不复用 payment_fundings.account_entry_id：那一行**只在结算成功的那个事务里**才存在，
-- 而这两列要覆盖的正是「结算没成功」的那个窗口——拿只在成功路径上出现的东西去描述失败路径，
-- 是做不到的。
--
-- 回填是空操作：这两列今天没有任何写入方，全部存量行都是 NULL，而 NULL 的含义恰好就是
-- 「这张单没有账户出资的扣减」（渠道支付永远没有）。不需要 DEFAULT。

ALTER TABLE payments ADD COLUMN account_entry_id UUID;
ALTER TABLE payments ADD COLUMN account_funded_at TIMESTAMPTZ;

COMMENT ON COLUMN payments.account_entry_id IS '账户出资扣豆的账变 ID（coffee_bean_entries.id），扣豆返回时即落库、先于结算；非空表示这笔钱确实动过，超时关单不会碰它，由补偿任务结算';
COMMENT ON COLUMN payments.account_funded_at IS '账户出资的成交时刻（账户域那笔账变的发生时刻）；渠道支付没有这一列的值，它由渠道给的 paid_at 描述';

-- 补偿任务扫「到点未结、但豆已经扣了」的那些。条件是 status ∈ (created, pending) 且
-- expires_at 已过且 account_entry_id 非空——三条都在索引里，扫描不落回全表。
-- 与 payments_pending_expiry_idx 同一个形状、同一个理由，只是又多带了一列做过滤。
CREATE INDEX payments_overdue_account_funding_idx ON payments (status, expires_at)
    WHERE status IN ('created', 'pending') AND account_entry_id IS NOT NULL;
