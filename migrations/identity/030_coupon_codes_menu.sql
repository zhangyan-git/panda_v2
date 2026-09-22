-- identity/030: 摘掉「优惠券码」这个点进去是空白页的侧栏菜单项
-- 可重复执行；只删菜单行，coupon:code:manage 权限保留（跟 008 保留 coupon:issue 同一口径）。
--
-- 007 插了六个子菜单，其中一个指向 /coupons/codes，可是 admin-web 的 .umirc.ts 里**没有这个
-- 路由**（四个子路由是 types / templates / batches / user-coupons，加 008 之后模板页还兼了发放）。
-- 所以这一项点下去是空白页：不是「页面还没做」而是「路由根本不存在」，404 都不给，直接白屏。
--
-- 「用户优惠券」那一项不在此列，它的路由是有的。
--
-- 只在 dev 库上被手工删过（所以线上那台今天看着是对的），007 仍是新库的起点，全新库跑完
-- 001→030 才不会有这一项——不补这条，任何新环境都会自动多出一个死链。
--
-- 不动的两处：
--   * coupon:code:manage 权限（007 建的）——权限码与页面是两回事，接口还在就留着，
--     摘菜单不摘权限是 008 定下的口径。
--   * admin_role_menus 的角色绑定——它按 menu_id 绑且是 ON DELETE CASCADE，跟着删干净。
--
-- 将来真做优惠券码页面时，照 007 的写法把菜单行加回去即可，不需要动这一条。

BEGIN;

DELETE FROM admin_menus WHERE path = '/coupons/codes';

COMMIT;
