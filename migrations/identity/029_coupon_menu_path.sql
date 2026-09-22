-- identity/029: 「优惠券管理」由带 path 的菜单行改成纯目录行（path 置空）
-- 可重复执行。
--
-- 007 建这一行时给的是 path = '/coupons'，而 admin-web 的 .umirc.ts 里 /coupons 是**没有
-- component 的父路由**——它只用来挂「优惠券类型 / 优惠券模板 / 发放批次 / 用户优惠券」四个子页。
-- 所以这个 path 指向的是一张空白页：侧栏点不进去（有子项时 ProLayout 渲染成子菜单），
-- 但地址栏敲进去内容区是白屏。
--
-- 其余八个目录行（系统管理、商户管理、设备管理、订单管理、抽奖管理、订货管理、会员管理、
-- 支付管理、开放平台）的 path 都是空串，是「只分组不跳转」的写法。这里按同一口径对齐。
--
-- 不动的两处：
--   * .umirc.ts 的 /coupons 路由——四个子页仍要以它为父路由，删了子页就没地方挂。
--   * admin_role_menus 的角色绑定——它按 menu_id 绑，改 path 不影响；且侧栏读的是
--     菜单树（见 sidebar-menus-are-bound-separately-from-permission-codes）。
--
-- 置空的副作用只有一个：不再有菜单项命中 /coupons，直接访问该地址时侧栏不点亮任何一项。
-- 那本来就是个不该存在的页面。

BEGIN;

UPDATE admin_menus SET path = '', updated_at = now()
WHERE parent_id IS NULL AND path = '/coupons';

COMMIT;
