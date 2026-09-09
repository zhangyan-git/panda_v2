import {
  DeleteOutlined,
  EditOutlined,
  MenuOutlined,
  PlusOutlined,
  SafetyCertificateOutlined,
} from '@ant-design/icons';
import {
  ModalForm,
  PageContainer,
  ProFormText,
  ProFormTextArea,
  ProTable,
} from '@ant-design/pro-components';
import { useAccess } from '@umijs/max';
import { Button, Checkbox, message, Modal, Popconfirm, Space, Tag, Tree } from 'antd';
import { useRef, useState } from 'react';
import type { ActionType, ProColumns } from '@ant-design/pro-components';
import type { DataNode } from 'antd/es/tree';
import {
  assignPermissionsToRole,
  createRole,
  deleteRole,
  listPermissions,
  listRolePermissions,
  listRoles,
  updateRole,
  type Permission,
  type Role,
} from '../../services/iam';
import { renderMenuIcon } from '../../menuIcons';
import {
  assignMenusToRole,
  listMenuTree,
  listRoleMenus,
  type MenuNode,
} from '../../services/menu';

const RolesPage: React.FC = () => {
  const access = useAccess();
  const actionRef = useRef<ActionType>();
  const [editing, setEditing] = useState<Role | null>(null);
  const [modalOpen, setModalOpen] = useState(false);

  // 分配权限 modal 状态
  const [permModal, setPermModal] = useState(false);
  const [permTarget, setPermTarget] = useState<Role | null>(null);
  const [allPerms, setAllPerms] = useState<Permission[]>([]);
  const [checkedPerms, setCheckedPerms] = useState<string[]>([]);

  const openPermModal = async (role: Role) => {
    setPermTarget(role);
    // 回显当前角色已绑定的权限；保存时整体覆盖
    const [perms, bound] = await Promise.all([listPermissions(), listRolePermissions(role.id)]);
    setAllPerms(perms);
    setCheckedPerms(bound.map((p) => p.id));
    setPermModal(true);
  };

  // 分配菜单 modal 状态
  const [menuModal, setMenuModal] = useState(false);
  const [menuTarget, setMenuTarget] = useState<Role | null>(null);
  const [menuTree, setMenuTree] = useState<MenuNode[]>([]);
  const [checkedMenus, setCheckedMenus] = useState<string[]>([]);

  const openMenuModal = async (role: Role) => {
    setMenuTarget(role);
    const [tree, checked] = await Promise.all([listMenuTree(), listRoleMenus(role.id)]);
    setMenuTree(tree);
    setCheckedMenus(checked);
    setMenuModal(true);
  };

  const toTreeData = (nodes: MenuNode[]): DataNode[] =>
    nodes.map((n) => ({
      key: n.id,
      title: (
        <Space size={4}>
          {renderMenuIcon(n.icon)}
          <span>{n.name}</span>
          {!n.path && <Tag style={{ marginInlineStart: 4 }}>目录</Tag>}
        </Space>
      ),
      children: n.children?.length ? toTreeData(n.children) : undefined,
    }));

  const columns: ProColumns<Role>[] = [
    { title: '角色代码', dataIndex: 'code', copyable: true, width: 160 },
    { title: '角色名称', dataIndex: 'name', width: 200, ellipsis: true },
    { title: '说明', dataIndex: 'description', ellipsis: true },
    {
      title: '创建时间',
      dataIndex: 'createdAt',
      valueType: 'dateTime',
      search: false,
      width: 180,
    },
    {
      title: '操作',
      valueType: 'option',
      width: 340,
      fixed: 'right',
      render: (_, row) => (
        <Space>
          {access.canWriteBindings && (
            <Button
              type="link"
              size="small"
              icon={<SafetyCertificateOutlined />}
              onClick={() => openPermModal(row)}
            >
              分配权限
            </Button>
          )}
          {access.canWriteBindings && (
            <Button
              type="link"
              size="small"
              icon={<MenuOutlined />}
              onClick={() => openMenuModal(row)}
            >
              分配菜单
            </Button>
          )}
          {access.canWriteRoles && (
            <Button
              type="link"
              size="small"
              icon={<EditOutlined />}
              onClick={() => {
                setEditing(row);
                setModalOpen(true);
              }}
            >
              编辑
            </Button>
          )}
          {access.canDeleteRoles && (
            <Popconfirm
              title="确认删除该角色？"
              onConfirm={async () => {
                await deleteRole(row.id);
                message.success('已删除');
                actionRef.current?.reload();
              }}
            >
              <Button type="link" size="small" danger icon={<DeleteOutlined />}>
                删除
              </Button>
            </Popconfirm>
          )}
        </Space>
      ),
    },
  ];

  // 按 group 分组显示权限
  const permGroups = allPerms.reduce<Record<string, Permission[]>>((acc, p) => {
    const g = p.group || '其他';
    (acc[g] ??= []).push(p);
    return acc;
  }, {});

  const allPermIds = allPerms.map((p) => p.id);
  const togglePerms = (ids: string[], checked: boolean) => {
    setCheckedPerms((prev) => {
      const next = new Set(prev);
      ids.forEach((id) => {
        if (checked) {
          next.add(id);
        } else {
          next.delete(id);
        }
      });
      return [...next];
    });
  };

  return (
    <PageContainer title="角色管理">
      <ProTable<Role>
        actionRef={actionRef}
        rowKey="id"
        columns={columns}
        scroll={{ x: 1180 }}
        request={async () => {
          const data = await listRoles();
          return { data, success: true };
        }}
        search={{ labelWidth: 'auto' }}
        toolBarRender={() => [
          access.canWriteRoles && (
            <Button
              key="add"
              type="primary"
              icon={<PlusOutlined />}
              onClick={() => {
                setEditing(null);
                setModalOpen(true);
              }}
            >
              新建角色
            </Button>
          ),
        ]}
      />

      {/* 新建 / 编辑角色 */}
      <ModalForm<{ code: string; name: string; description?: string }>
        title={editing ? '编辑角色' : '新建角色'}
        open={modalOpen}
        onOpenChange={(v) => {
          setModalOpen(v);
          if (!v) setEditing(null);
        }}
        initialValues={
          editing
            ? {
                code: editing.code,
                name: editing.name,
                description: editing.description,
              }
            : undefined
        }
        onFinish={async (values) => {
          if (editing) {
            await updateRole(editing.id, values);
            message.success('已更新');
          } else {
            await createRole(values);
            message.success('已创建');
          }
          actionRef.current?.reload();
          return true;
        }}
      >
        <ProFormText
          name="code"
          label="角色代码"
          placeholder="如 operator"
          rules={[{ required: true, message: '请输入角色代码' }]}
        />
        <ProFormText
          name="name"
          label="角色名称"
          placeholder="如 运营人员"
          rules={[{ required: true, message: '请输入角色名称' }]}
        />
        <ProFormTextArea name="description" label="说明" fieldProps={{ rows: 2 }} />
      </ModalForm>

      {/* 分配菜单 */}
      <Modal
        title={`为「${menuTarget?.name || menuTarget?.code}」分配菜单`}
        open={menuModal}
        onCancel={() => setMenuModal(false)}
        onOk={async () => {
          if (!menuTarget) return;
          await assignMenusToRole(menuTarget.id, checkedMenus);
          message.success('菜单已更新');
          setMenuModal(false);
        }}
        width={480}
      >
        <Tree
          checkable
          defaultExpandAll
          treeData={toTreeData(menuTree)}
          checkedKeys={checkedMenus}
          onCheck={(keys) => setCheckedMenus(keys as string[])}
          style={{ maxHeight: 400, overflow: 'auto' }}
        />
      </Modal>

      {/* 分配权限 */}
      <Modal
        title={`为「${permTarget?.name || permTarget?.code}」分配权限`}
        open={permModal}
        onCancel={() => setPermModal(false)}
        onOk={async () => {
          if (!permTarget) return;
          await assignPermissionsToRole(permTarget.id, checkedPerms);
          message.success('权限已更新');
          setPermModal(false);
        }}
        width={600}
      >
        <div style={{ marginBottom: 12 }}>
          <Checkbox
            indeterminate={checkedPerms.length > 0 && checkedPerms.length < allPermIds.length}
            checked={allPermIds.length > 0 && checkedPerms.length === allPermIds.length}
            onChange={(e) => togglePerms(allPermIds, e.target.checked)}
          >
            全选（已选 {checkedPerms.length}/{allPermIds.length}）
          </Checkbox>
        </div>
        {Object.entries(permGroups).map(([group, perms]) => {
          const groupIds = perms.map((p) => p.id);
          const checkedInGroup = groupIds.filter((id) => checkedPerms.includes(id)).length;
          return (
            <div key={group} style={{ marginBottom: 16 }}>
              <div
                style={{
                  display: 'flex',
                  alignItems: 'center',
                  justifyContent: 'space-between',
                  marginBottom: 8,
                }}
              >
                <Tag color="blue" style={{ marginInlineEnd: 0 }}>
                  {group}
                </Tag>
                <Checkbox
                  indeterminate={checkedInGroup > 0 && checkedInGroup < groupIds.length}
                  checked={groupIds.length > 0 && checkedInGroup === groupIds.length}
                  onChange={(e) => togglePerms(groupIds, e.target.checked)}
                >
                  全选
                </Checkbox>
              </div>
              <Checkbox.Group
                value={checkedPerms}
                onChange={(vals) => setCheckedPerms(vals as string[])}
                style={{ display: 'flex', flexWrap: 'wrap', gap: 8 }}
              >
                {perms.map((p) => (
                  <Checkbox key={p.id} value={p.id} style={{ marginInlineStart: 0 }}>
                    <span style={{ fontSize: 13 }}>{p.name}</span>
                    <code style={{ fontSize: 11, color: 'oklch(0.5 0 0)', marginLeft: 4 }}>
                      {p.code}
                    </code>
                  </Checkbox>
                ))}
              </Checkbox.Group>
            </div>
          );
        })}
      </Modal>
    </PageContainer>
  );
};

export default RolesPage;
