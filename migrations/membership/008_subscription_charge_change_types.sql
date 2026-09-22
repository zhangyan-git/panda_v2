-- membership/008：代扣这一刀要写的两种变更类型
--
-- 001 给 membership_changes.change_type 定的取值表是十二个，代扣失败与订阅暂停两条流水
-- **都不在里面**。不补的话，事件消费会走到 CHECK 上被拒——表现是「渠道说这期钱没扣到，
-- 本地什么都没发生、日志里只有一句约束冲突」，而用户那边看到的仍是「自动续费：已开启」。
-- 这两种变更又恰恰是这一刀里唯一会无限重复发生的两条，放不进流水就只能靠翻日志排查。
--
-- # 为什么成功的那条不在这里
--
-- 扣款成功写的是**已有的 `renew`**，没有 `charge_succeeded` 这个值。一次成功的代扣与一次
-- 支付成功在会员身上是同一件事（有效期后移一个周期），对用户时间线而言也是同一句话。给代扣
-- 单开一个类型，只会让「这个月是怎么续上的」在时间线上分成两种长得一样的条目，而分不分得清
-- 靠的是 change_type 之外的东西（metadata 里的协议号）——那才是该去看的地方。
--
-- # suspend 不是 freeze
--
-- `freeze` 是**权益暂停**（会员卡被冻住了，用户还能做什么照旧另说），而这里是**代扣被停掉**
-- ——连续几期扣不到钱之后，本服务不再自动发起下一期，会员本身照常到期失效。两者的操作人、
-- 可恢复方式、用户该看到的提示都不一样，所以不能共用一个值。
--
-- 与之相关的一条**有意为之**：暂停不是解约。渠道那边的协议仍然挂着，用户如果在暂停期间自己
-- 去微信里看，看到的是「已签约」。本服务不替用户撤授权（见 §六.4）——那是要用户自己点、
-- 或者运营在后台确认过的事。
--
-- 反过来它也**不是终态**：用户重新充上钱、运营在后台恢复，都能把订阅推回 active，此后每期
-- 又是一条 `renew`。流水是只增不改的，所以「这中间停过一段」只能靠这两条 suspend/续上的
-- 记录还原，不能靠订阅行的当前状态。

ALTER TABLE membership_changes DROP CONSTRAINT membership_changes_change_type_check;

ALTER TABLE membership_changes ADD CONSTRAINT membership_changes_change_type_check
    CHECK (change_type IN (
        'activate',        -- 开通（首购生效）
        'renew',           -- 续费（有效期叠加）；**代扣成功也写这个**
        'expire',          -- 到期失效
        'freeze',          -- 权益暂停
        'unfreeze',        -- 暂停恢复
        'auto_renew_on',   -- 用户打开自动续费
        'auto_renew_off',  -- 用户关闭自动续费
        'subscribe',       -- 签约连续包月
        'unsubscribe',     -- 解约
        'refund_adjust',   -- 退款后按规则调整权益
        'admin_adjust',    -- 后台人工调整
        'revoke',          -- 撤销会员
        'charge_failed',   -- 一期代扣没扣到（这一期还没成，到期日不动）
        'suspend'          -- 连续失败到上限，停掉自动续费；协议还在，不是解约
    ));
