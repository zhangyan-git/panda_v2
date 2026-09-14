-- 007: 平台权限动作统一为 view / manage / delete
--
-- 背景：admin_permissions 的动作词表原本混用两套——users / permissions / roles 同时存在
-- read/write 与 view/manage 两组码（组内显示名完全重复，且只有 read/write 被
-- services/user-service/cmd/main.go 与 merchant-service 的权限表引用），其余资源只有
-- read/write。本迁移统一为 view / manage / delete，与 merchant_permissions 的词表一致
-- （见 001_init.sql 对 merchant_permissions.action 的注释：view、edit、delete、export、manage）。
--
-- 步骤：
--   1. 为 bindings / brands / menus / merchants / stores 补齐 view、manage 权限码
--   2. 已有角色绑定（admin_role_permissions）与平台域 Casbin 策略（casbin_rule，
--      ptype='p' 且 v1=''）从 read/write 改指 view/manage
--   3. 删除 read/write 权限行（角色绑定随外键级联删除）
--
-- 不改 002/003/005 这些已应用的迁移：新库按 001→007 顺序执行后同样收敛到 view/manage。
-- casbin_rule 只有主键、没有 (ptype,v0..v5) 唯一约束，因此补规则必须用 NOT EXISTS 防重。

BEGIN;

-- 1) 补齐缺失的 view / manage 权限码
INSERT INTO admin_permissions (id, code, resource, action, perm_group, name, description, created_at)
VALUES
  (gen_random_uuid(), 'admin:bindings:view',    'admin_bindings', 'view',   'bindings',  '查看绑定', '查看角色权限与用户角色绑定', NOW()),
  (gen_random_uuid(), 'admin:bindings:manage',  'admin_bindings', 'manage', 'bindings',  '管理绑定', '调整角色权限与用户角色绑定', NOW()),
  (gen_random_uuid(), 'admin:menus:view',       'menus',          'view',   'menus',     '查看菜单', '查看后台菜单树', NOW()),
  (gen_random_uuid(), 'admin:menus:manage',     'menus',          'manage', 'menus',     '管理菜单', '新建/编辑后台菜单', NOW()),
  (gen_random_uuid(), 'admin:merchants:view',   'merchants',      'view',   'merchants', '查看商户', '查看商户列表与商户账号', NOW()),
  (gen_random_uuid(), 'admin:merchants:manage', 'merchants',      'manage', 'merchants', '管理商户', '新建/编辑商户、状态流转、维护商户账号', NOW()),
  (gen_random_uuid(), 'admin:brands:view',      'brands',         'view',   'brands',    '查看品牌', '查看品牌列表', NOW()),
  (gen_random_uuid(), 'admin:brands:manage',    'brands',         'manage', 'brands',    '管理品牌', '新建/编辑品牌、启用禁用、审核', NOW()),
  (gen_random_uuid(), 'admin:stores:view',      'stores',         'view',   'stores',    '查看门店', '查看门店列表', NOW()),
  (gen_random_uuid(), 'admin:stores:manage',    'stores',         'manage', 'stores',    '管理门店', '新建/编辑门店、启用禁用、审核', NOW())
ON CONFLICT (code) DO NOTHING;

-- 2) users 分组名与其它资源统一（原为 admin-users）
UPDATE admin_permissions SET perm_group = 'users' WHERE perm_group = 'admin-users';

-- 3) 角色绑定改指 view / manage
INSERT INTO admin_role_permissions (role_id, permission_id)
SELECT DISTINCT rp.role_id, np.id
FROM admin_role_permissions rp
JOIN admin_permissions op ON op.id = rp.permission_id
JOIN admin_permissions np
  ON np.code = CASE WHEN op.code ~ ':read$'  THEN regexp_replace(op.code, ':read$',  ':view')
                    WHEN op.code ~ ':write$' THEN regexp_replace(op.code, ':write$', ':manage')
               END
WHERE op.code ~ '^admin:(users|roles|permissions|bindings|menus|merchants|brands|stores):(read|write)$'
ON CONFLICT (role_id, permission_id) DO NOTHING;

-- 4) 平台域 Casbin 策略改指 view / manage
INSERT INTO casbin_rule (ptype, v0, v1, v2, v3)
SELECT DISTINCT 'p', c.v0, c.v1,
       CASE WHEN c.v2 ~ ':read$'  THEN regexp_replace(c.v2, ':read$',  ':view')
            WHEN c.v2 ~ ':write$' THEN regexp_replace(c.v2, ':write$', ':manage')
       END,
       c.v3
FROM casbin_rule c
WHERE c.ptype = 'p' AND c.v1 = ''
  AND c.v2 ~ '^admin:(users|roles|permissions|bindings|menus|merchants|brands|stores):(read|write)$'
  AND NOT EXISTS (
    SELECT 1 FROM casbin_rule e
    WHERE e.ptype = 'p' AND e.v0 = c.v0 AND e.v1 = c.v1 AND e.v2 =
          CASE WHEN c.v2 ~ ':read$'  THEN regexp_replace(c.v2, ':read$',  ':view')
               WHEN c.v2 ~ ':write$' THEN regexp_replace(c.v2, ':write$', ':manage')
          END);

DELETE FROM casbin_rule
WHERE ptype = 'p' AND v1 = ''
  AND v2 ~ '^admin:(users|roles|permissions|bindings|menus|merchants|brands|stores):(read|write)$';

-- 5) 删除 read / write 权限行
DELETE FROM admin_permissions
WHERE code ~ '^admin:(users|roles|permissions|bindings|menus|merchants|brands|stores):(read|write)$';

COMMIT;
