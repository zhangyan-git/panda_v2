-- 008: 权限分组名改为中文显示名
--
-- 背景：admin_permissions.perm_group 一直被当作展示用的分组名，权限管理页的「分组」表单项
-- 占位文案就是「如 角色管理」，但 001~007 写入的都是 brands / menus 这类英文 slug，
-- 于是「分配权限」弹窗与权限列表里露出的都是英文分组标签。
--
-- 该字段只用于展示与排序（repository 里 ORDER BY perm_group, code），不参与任何鉴权判断，
-- 因此直接改成中文显示名，不额外引入前端映射表（避免两处维护、新分组漏翻）。
--
-- 不改 002/003/005/007 这些已应用的迁移：新库按 001→008 顺序执行后同样收敛到中文分组。

BEGIN;

UPDATE admin_permissions
SET perm_group = CASE perm_group
  WHEN 'users'       THEN '用户管理'
  WHEN 'roles'       THEN '角色管理'
  WHEN 'permissions' THEN '权限管理'
  WHEN 'bindings'    THEN '绑定管理'
  WHEN 'menus'       THEN '菜单管理'
  WHEN 'merchants'   THEN '商户管理'
  WHEN 'brands'      THEN '品牌管理'
  WHEN 'stores'      THEN '门店管理'
END
WHERE perm_group IN ('users', 'roles', 'permissions', 'bindings', 'menus', 'merchants', 'brands', 'stores');

COMMIT;
