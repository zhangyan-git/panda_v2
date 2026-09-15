-- account/006：咖啡豆账户与流水的中文注释

COMMENT ON TABLE coffee_bean_accounts IS '用户维度的咖啡豆账户（方案 5.6 的账户余额那一半），一个用户一行、懒创建；单位为分。与 panda_coffee_machine.devices.coffee_balance（设备维度的预付余额）无关';
COMMENT ON COLUMN coffee_bean_accounts.user_id IS '账户归属的用户，跨库值引用（用户在身份库）；本库不建跨库外键';
COMMENT ON COLUMN coffee_bean_accounts.balance IS '咖啡豆余额，单位为分，与 orders.payable_amount 同单位；是流水求和的结果，与前一条流水在同一次事务里更新。CHECK (balance >= 0) 是扣减与负数调整的最后一道防线';
COMMENT ON COLUMN coffee_bean_accounts.created_at IS '账户行创建时间（第一次调整或第一次扣减时）';
COMMENT ON COLUMN coffee_bean_accounts.updated_at IS '最近一次余额变动时间';

COMMENT ON TABLE coffee_bean_entries IS '咖啡豆账变流水，只允许追加；冲正是新增一条反向记录而不是原地改数。没有冻结、没有过期';
COMMENT ON COLUMN coffee_bean_entries.entry_type IS '账变类型：adjust=后台人工调整（充值或纠错），consume=纯豆出资扣减，reverse=冲正';
COMMENT ON COLUMN coffee_bean_entries.amount IS '变动额，单位分，有符号：充值为正、纠错为负、扣减为负、冲正与它冲掉的那笔相反';
COMMENT ON COLUMN coffee_bean_entries.balance_after IS '变动后余额，明细页显示的就是它';
COMMENT ON COLUMN coffee_bean_entries.title IS '这一行在人那边显示成什么（后台充值 / 咖啡豆支付 / 退款冲正），文案由本服务拥有';
COMMENT ON COLUMN coffee_bean_entries.reference_type IS '起因对象的类型：manual=后台调整，order=纯豆出资扣减，after_sale=退款冲正';
COMMENT ON COLUMN coffee_bean_entries.reference_id IS '起因对象的 ID，跨库值引用（订单 ID / 售后单 ID）';
COMMENT ON COLUMN coffee_bean_entries.reference_no IS '给人看的号：调整的幂等号、订单号、售后单号';
COMMENT ON COLUMN coffee_bean_entries.entry_key IS '幂等键，唯一索引 coffee_bean_entries_key_unique。三种形状：adjust:{requestId}（后台调整）、order:{orderId}（纯豆扣减，用订单 ID 是因为冲正要按订单反查）、after_sale:{afterSaleNo}（冲正，一条售后只冲一次）';
COMMENT ON COLUMN coffee_bean_entries.reverses_entry_id IS '被冲正的那条流水，只有 entry_type=reverse 有值；与 amount 的符号一起构成「冲正不原地改数」';
COMMENT ON COLUMN coffee_bean_entries.operator_id IS '后台调整的操作人 ID，只对 entry_type=adjust 有值；是审计之外的第二处留痕';
COMMENT ON COLUMN coffee_bean_entries.operator_name IS '后台调整的操作人显示名。今天恒为空串，与 device_balance_ledger.operator_name 一样：令牌里没有用户名，展示名由读侧按 operator_id 去身份库解析';
COMMENT ON COLUMN coffee_bean_entries.remark IS '备注；后台调整时是操作人填的理由';
COMMENT ON COLUMN coffee_bean_entries.occurred_at IS '业务发生时间：调整用请求时刻，扣减用支付时刻，冲正用审核时刻；用事件时刻而不是 NOW()，补投旧事件时明细顺序才不变';
COMMENT ON COLUMN coffee_bean_entries.created_at IS '入库时间';
