-- identity/036: 收掉订货 / 库存域（inventory-service）留下的菜单与权限
-- 可重复执行。
--
-- 整个库存域从 V2 里删掉了：backend/services/inventory-service/ 整个目录、
-- contracts/proto/inventory/、migrations/inventory/ 四份迁移、网关那条上游、admin-web 的七页
-- 与 services/inventory.ts 一起没了。这一门域**将来要单独拆一个服务出去**，所以不是
-- 「先摘入口、接口留着」——那里已经一条接口都不剩（网关的 inventory 上游、config 里的
-- INVENTORY_DATABASE_URL、compose 里那个 service 全删了），与 032 收「支付方式与渠道」是同
-- 一种情形：连接口带页面整条配置面都没了，留着权限码只会让权限页上多四行「勾了没有任何用」。
--
-- 数据库这一半要跟上，否则留下两样只有升级那台机器上看得见的东西：
--
--   * 菜单「订货管理」整棵子树（一个目录节点 + 七条叶子）—— 侧栏是从 admin_menus 来的
--     （menuDataRender 整个替换掉静态路由里的菜单项），删了页面不删菜单，那七项点下去是空白
--     页：不是「页面还没做」而是「路由根本不存在」，404 都不给（030 摘 /coupons/codes、
--     032 摘 /payments/methods 是同一种病）。而这一次更彻底——目录节点自己也要删，否则侧栏上
--     会留一个**空的**「订货管理」分组头（path='' 的目录节点不跳转，删掉叶子之后它就只是个
--     没有子项的标题）。
--   * 权限码 inventory:read / manage / receipt / issue —— 023 建了前三枚、024 建了第四枚，
--     理由各写在那个文件里（manage 改基础资料、receipt/issue 动结余与流水，刻意不合并）。
--     那些理由跟着服务一起失效了：今天没有任何接口认这四枚码。
--
-- admin_role_permissions 与 admin_role_menus 的外键都是 ON DELETE CASCADE（001），绑定跟着删。
--
-- # 与 008 / 030 的区别（别照抄那两次）
--
-- 那两次摘的是**页面的入口**而接口都还在，所以码留着；这次连接口带页面整条配置面都没了，
-- 与 032 同一条判断。
--
-- # 一个已知的副作用（接受）
--
-- 已经拿这四枚码勾过的角色（今天只有 super_admin，见 023/024）在删掉那一刻失去它们——这正是
-- 想要的。但**审计里那几条库存写操作记录不回滚**：admin_operation_logs 里的 action 与操作人
-- 照旧在，只是那些接口今天已经不存在了。那份日志的价值在于「当时发生过什么」，它不该被后来的
-- 删除改写（032 同一条）。
--
-- # 数据不动
--
-- panda_inventory 那个库、库里的表与数据、以及它的 schema_migrations 行**都不动**：迁移只
-- 管身份库里的菜单与权限。删掉的四份迁移文件不会再被 Apply 读到（migrate.Apply 只遍历
-- embed FS 里现存的文件，已记下的版本号它不回查），所以那几行是惰性的孤儿，不影响启动。
--
-- # 文件留下，不改 023 / 024
--
-- 那两份已经 apply 过，历史不该被改写：一个全新的库按顺序跑到这里，同样是先建后删、结果一致。

BEGIN;

-- 一、摘掉七条叶子（/inventory/materials、recipes、warehouses、stock-levels、
-- stock-movements、receipts、issues）。admin_role_menus 跟着级联删。
DELETE FROM admin_menus WHERE path LIKE '/inventory/%';

-- 二、再摘目录节点。它没有 path（023 特意留空——path 不为空的目录会被 ProLayout 当成可跳转的
-- 菜单项渲染），所以只能按名字 + 顶层来认，与 023 建它时那条 WHERE 逐字对应。
DELETE FROM admin_menus
WHERE parent_id IS NULL AND path = '' AND name = '订货管理';

-- 三、收回四枚已经没有接口认的码（admin_role_permissions 跟着级联删）。
DELETE FROM admin_permissions
WHERE code IN ('inventory:read', 'inventory:manage', 'inventory:receipt', 'inventory:issue');

COMMIT;
