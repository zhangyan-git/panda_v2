-- identity/007: 优惠券后台权限与菜单
-- 可重复执行；仅为 super_admin 绑定优惠券菜单和权限。

BEGIN;

INSERT INTO admin_permissions (id, code, perm_group, name, description)
VALUES
  (gen_random_uuid(), 'coupon:read', '优惠券管理', '查看优惠券', '查看优惠券管理页面与数据'),
  (gen_random_uuid(), 'coupon:type:manage', '优惠券管理', '管理优惠券类型', '创建、编辑和停用优惠券类型'),
  (gen_random_uuid(), 'coupon:template:manage', '优惠券管理', '管理优惠券模板', '创建、编辑和管理优惠券模板'),
  (gen_random_uuid(), 'coupon:template:audit', '优惠券管理', '审核优惠券模板', '审核优惠券模板'),
  (gen_random_uuid(), 'coupon:issue', '优惠券管理', '发放优惠券', '向用户批量发放优惠券'),
  (gen_random_uuid(), 'coupon:issue:override', '优惠券管理', '绕过发券限制', '允许发券时跳过用户领取限制'),
  (gen_random_uuid(), 'coupon:user-coupon:read', '优惠券管理', '查看用户券', '查看用户优惠券及详情'),
  (gen_random_uuid(), 'coupon:user-coupon:redeem', '优惠券管理', '核销用户券', '管理员核销用户优惠券'),
  (gen_random_uuid(), 'coupon:user-coupon:revoke', '优惠券管理', '撤销用户券', '管理员撤销用户优惠券'),
  (gen_random_uuid(), 'coupon:batch:manage', '优惠券管理', '管理发券批次', '查看发券批次和统计'),
  (gen_random_uuid(), 'coupon:code:manage', '优惠券管理', '管理优惠券码', '管理外部券码和实体券批次'),
  (gen_random_uuid(), 'admin:coupons:manage', '优惠券管理', '管理优惠券（兼容）', '兼容现有优惠券服务权限码'),
  (gen_random_uuid(), 'admin:coupons:issue', '优惠券管理', '发放优惠券（兼容）', '兼容现有优惠券服务权限码')
ON CONFLICT (code) DO NOTHING;

INSERT INTO admin_role_permissions (role_id, permission_id)
SELECT r.id, p.id
FROM admin_roles r
CROSS JOIN admin_permissions p
WHERE r.code = 'super_admin'
  AND p.code IN (
    'coupon:read', 'coupon:type:manage', 'coupon:template:manage',
    'coupon:template:audit', 'coupon:issue', 'coupon:issue:override',
    'coupon:user-coupon:read', 'coupon:user-coupon:redeem',
    'coupon:user-coupon:revoke', 'coupon:batch:manage', 'coupon:code:manage',
    'admin:coupons:manage', 'admin:coupons:issue'
  )
ON CONFLICT (role_id, permission_id) DO NOTHING;

INSERT INTO admin_menus (name, path, icon, sort)
SELECT '优惠券管理', '/coupons', 'GiftOutlined', 4
WHERE NOT EXISTS (
  SELECT 1 FROM admin_menus WHERE parent_id IS NULL AND path = '/coupons'
);

INSERT INTO admin_menus (parent_id, name, path, icon, sort)
SELECT p.id, v.name, v.path, v.icon, v.sort
FROM admin_menus p
CROSS JOIN (VALUES
  ('优惠券类型', '/coupons/types', 'TagsOutlined', 1),
  ('优惠券模板', '/coupons/templates', 'FileTextOutlined', 2),
  ('发放优惠券', '/coupons/issue', 'SendOutlined', 3),
  ('发放批次', '/coupons/batches', 'UnorderedListOutlined', 4),
  ('用户优惠券', '/coupons/user-coupons', 'UserOutlined', 5),
  ('优惠券码', '/coupons/codes', 'BarcodeOutlined', 6)
) AS v(name, path, icon, sort)
WHERE p.path = '/coupons'
  AND NOT EXISTS (SELECT 1 FROM admin_menus m WHERE m.path = v.path);

INSERT INTO admin_role_menus (role_id, menu_id)
SELECT r.id, m.id
FROM admin_roles r
CROSS JOIN admin_menus m
WHERE r.code = 'super_admin'
  AND (m.path = '/coupons' OR m.path LIKE '/coupons/%')
ON CONFLICT (role_id, menu_id) DO NOTHING;

COMMIT;
