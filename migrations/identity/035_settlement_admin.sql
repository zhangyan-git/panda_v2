-- identity/035: 分账后台权限与菜单
-- 可重复执行；仅为 super_admin 绑定，其余角色去角色管理里勾。
--
-- 两个码，按动作拆：
--   settlement:read    看分账规则、看收款账户、看分账明细（任务与接收方）。
--   settlement:manage  建改删分账规则与收款账户 —— 也就是「接下来这笔钱怎么分、分给谁」。
--
-- **它们绝不并进 payment:read / payment:manage**，这是 008 的文件头（第 11–13 行）定死的三分：
-- 「谁能看支付单」与「谁能改分账配置」是两件事。前者是所有客服都要的（用户问「我这笔钱扣了没」
-- 就得看），后者决定了钱分给谁。并进去之后，一次「给他开个支付单查询吧」会连带把分账的写权限
-- 一起给出去，而那是这个后台里最不该顺手送人的一枚。
--
-- 注意 payment:manage 今天**不存在**（032 把它连同「支付方式与渠道」那一页收掉了，因为方式与
-- 渠道已经是代码里的常量、运营没有可配的东西）。所以这里不是「在已有的 manage 下加一枚」，
-- 而是本域第一次出现写码——payment:read 仍然只管看支付单。
--
-- # 三分里的第三枚（settlement:payout）为什么今天不发
--
-- 008 那一行写的是三分：看 / 配 / 打款。第三枚今天**一格接口都没有**：分账在 V2 是**随支付一次
-- 下发**的（银联商务的 divisionFlag + subOrders 拼在下单报文里，支付成功即分账成功），没有一个
-- 「向渠道发起打款」的动作可以授权，本地也没有结算单与打款那一层——老系统同样没有。
--
-- 所以按 026 对 payment:manage 的同一条理由不发它：发一个今天永远勾不出对应行为、勾了也没有
-- 任何接口认的码，只会让权限页在骗人。真要发的那一天，是「结算单与打款」这件事真做出来的那一天。
--
-- 菜单：目录「支付管理」下面加三页叶子。目录是 026 建的（path='' 的空路径分组节点），这里
-- **只加叶子**，不动目录行本身。
--   * 分账规则 /payments/settlement-rules —— 配「这类业务、这个范围上的钱怎么分」。
--   * 分账账户 /payments/settlement-accounts —— 配「钱分到谁的哪个子商户号上」。
--   * 分账明细 /payments/settlement-tasks —— 只读，看已经发生的那一笔分给了谁多少。
--
-- 三页都留在「支付管理」下而不是单开一个目录：它们离开那一笔支付就没有意义（规则与账户是
-- 「这笔钱怎么分」的两半，明细是这笔钱分完的样子），与 033 的包月订阅挂在会员管理下同一条道理。
-- 分账任务的详情不单独挂菜单，与 023 的入库单详情形同。
--
-- sort 从 2 往末尾排。026 建的「支付单」是 1，「支付方式与渠道」是 2 且已被 032 删掉，所以
-- 2/3/4 正好接在剩下那一页后面。**不要插到中间去**——那会让所有人的侧边栏顺序在升级那一刻
-- 整体挪位（026 第 47–48 行的同一条注意事项）。
--
-- 图标名要与 admin-web 的 src/menuIcons.tsx 登记的名字**逐字相同**，否则 renderMenuIcon 认不出
-- 来、返回 undefined，侧栏那格空着且不报错（022 漏过一次，026 后来也补过一条注意事项）。
-- PartitionOutlined / BankOutlined / ReconciliationOutlined 三个本次已同时登记。

BEGIN;

INSERT INTO admin_permissions (id, code, perm_group, name, description)
VALUES
  (gen_random_uuid(), 'settlement:read', '支付管理', '查看分账配置与明细',
   '查看分账规则、收款账户与分账明细（每条任务分给了谁、分了多少）；只读，不包含任何修改分账配置的能力'),
  (gen_random_uuid(), 'settlement:manage', '支付管理', '管理分账规则与收款账户',
   '新增 / 修改 / 删除分账规则与收款账户，包括每一档分法的比例、固定额与收款方；决定的是「接下来这笔钱怎么分」，已经发生的分账不受影响')
ON CONFLICT (code) DO NOTHING;

-- 超管在中间件里直接放行，这里补绑定是为了前端 access 与权限回显能拿到码。
INSERT INTO admin_role_permissions (role_id, permission_id)
SELECT r.id, p.id
FROM admin_roles r
CROSS JOIN admin_permissions p
WHERE r.code = 'super_admin'
  AND p.code IN ('settlement:read', 'settlement:manage')
ON CONFLICT (role_id, permission_id) DO NOTHING;

INSERT INTO admin_menus (parent_id, name, path, icon, sort)
SELECT p.id, v.name, v.path, v.icon, v.sort
FROM admin_menus p
CROSS JOIN (VALUES
  ('分账规则', '/payments/settlement-rules', 'PartitionOutlined', 2),
  ('分账账户', '/payments/settlement-accounts', 'BankOutlined', 3),
  ('分账明细', '/payments/settlement-tasks', 'ReconciliationOutlined', 4)
) AS v(name, path, icon, sort)
WHERE p.path = ''
  AND p.name = '支付管理'
  AND p.parent_id IS NULL
  AND NOT EXISTS (SELECT 1 FROM admin_menus m WHERE m.path = v.path);

-- 菜单绑定。
--
-- **绑定必须排在插入之后**（033 第 30–31 行那条教训）：026 那次 `LIKE '/payments/%'` 的绑定是它
-- 自己执行那一刻的快照，今天新插进来的三行不在里面。同一个角色、同一条规则，这里只补新路径。
--
-- 三条一起写而不是逐条列：它们同属这一刀，将来加第四条时也照这条规则补，别回头去改 026。
INSERT INTO admin_role_menus (role_id, menu_id)
SELECT r.id, m.id
FROM admin_roles r
CROSS JOIN admin_menus m
WHERE r.code = 'super_admin'
  AND m.path IN ('/payments/settlement-rules', '/payments/settlement-accounts',
                 '/payments/settlement-tasks')
ON CONFLICT (role_id, menu_id) DO NOTHING;

COMMIT;
