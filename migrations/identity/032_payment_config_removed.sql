-- identity/032: 收掉「支付方式与渠道」那一页留下的菜单与权限
-- 可重复执行。
--
-- 那一页连同 payment-service 的六个写接口一起删了：收钱那四种（咖啡豆 / 银联商务小程序 /
-- 银联商务 H5 / 取货码）现在写在 payment-service 的 provider/catalog.go 里，密钥走环境变量，
-- 运营没有可配的东西。后台也就没有可写的表单——admin-web 的 src/pages/payments/methods/
-- 整个目录、.umirc.ts 里那条路由、services/payment.ts 里那八个接口一起没了。
--
-- 数据库这一半要跟上，否则留下两样只有升级那台机器上看得见的东西：
--
--   * 菜单叶子 /payments/methods —— 侧栏是从 admin_menus 来的（menuDataRender 整个替换掉
--     静态路由里的菜单项），删了页面不删菜单，那一项点下去是空白页：不是「页面还没做」而是
--     「路由根本不存在」，404 都不给（030 摘 /coupons/codes 是同一种病）。
--   * 权限码 payment:manage —— 026 建这枚的时候刻意空着不发，理由写在那边：「发一个今天
--     永远勾不出对应行为、勾了也没有任何接口认的 manage 码，只会让权限页在骗人」。027 是
--     因为它真有行为了才发出来（六个写接口），现在那些接口没了，这枚码又变回了那句话说的
--     东西，所以按同一条理由收回。
--
-- 与 008 / 030「摘菜单不摘权限」不是同一个情形，别照抄：那两次摘的是**页面的入口**，而接口
-- （发券、优惠券码）都还在，所以码留着；这次连接口带页面整条配置面都没了，留着这枚码只会让
-- 权限页上多一行「勾了没有任何用」。
--
-- admin_role_permissions 与 admin_role_menus 的外键都是 ON DELETE CASCADE（001），绑定跟着删。
--
-- # 一个已知的副作用（接受）
--
-- 已经拿这枚码勾过的角色（今天只有 super_admin，见 027）在删掉那一刻失去它——这正是想要的。
-- 但**审计里那几条 027 之后的写操作记录不回滚**：admin_operation_logs 里的 action 与操作人
-- 照旧在，只是那个 action 对应的接口今天已经不存在了。那份日志的价值在于「当时发生过什么」，
-- 它不该被后来的删除改写。
--
-- # 剩下的那一枚码
--
-- payment:read 保留（支付单列表与详情仍然只有这一条路能看），但它的说明里写着「看支付方式与
-- 渠道的配置」，那句今天指不到任何东西了，一并改掉。
--
-- 图片/文案不动：026 建的目录行「支付管理」与叶子「支付单」保持原样，图标与排序都不动。

BEGIN;

-- 一、摘掉那一页的菜单叶子（admin_role_menus 跟着级联删）。
DELETE FROM admin_menus WHERE path = '/payments/methods';

-- 二、收回那枚已经没有接口认的写码（admin_role_permissions 跟着级联删）。
DELETE FROM admin_permissions WHERE code = 'payment:manage';

-- 三、read 的说明里去掉了那句指向已删页面的描述。
UPDATE admin_permissions
SET name = '查看支付单',
    description = '查看支付单列表与详情（出资行、记账流水、状态流转、渠道调用、回调通知）；只读，不包含任何发起、关单或退款能力'
WHERE code = 'payment:read';

COMMIT;
