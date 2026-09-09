import {
  PlusOutlined,
  SafetyCertificateOutlined,
  StopOutlined,
  CheckCircleOutlined,
} from '@ant-design/icons';
import {
  ModalForm,
  PageContainer,
  ProFormText,
  ProTable,
} from '@ant-design/pro-components';
import { useAccess } from '@umijs/max';
import { Button, message, Modal, Popconfirm, Space, Tag, Transfer } from 'antd';
import { useEffect, useRef, useState } from 'react';
import type { ActionType, ProColumns } from '@ant-design/pro-components';
import type { TransferProps } from 'antd';
import {
  assignRolesToUser,
  createAdminUser,
  listAdminUsers,
  listRoles,
  listUserRoles,
  updateAdminUserStatus,
  type AdminUser,
  type Role,
} from '../../services/iam';

const AdminUsersPage: React.FC = () => {
  const access = useAccess();
  const actionRef = useRef<ActionType>();
  const [createOpen, setCreateOpen] = useState(false);

  // 分配角色 modal
  const [roleModal, setRoleModal] = useState(false);
  const [roleTarget, setRoleTarget] = useState<AdminUser | null>(null);
  const [allRoles, setAllRoles] = useState<Role[]>([]);
  const [targetKeys, setTargetKeys] = useState<string[]>([]);

  const openRoleModal = async (user: AdminUser) => {
    setRoleTarget(user);
    const [roles, userRoles] = await Promise.all([listRoles(), listUserRoles(user.id)]);
    setAllRoles(roles);
    setTargetKeys(userRoles.map((r) => r.id));
    setRoleModal(true);
  };

  const columns: ProColumns<AdminUser>[] = [
    { title: '用户名', dataIndex: 'username', copyable: true, width: 160 },
    { title: '姓名', dataIndex: 'name', width: 120 },
    { title: '邮箱', dataIndex: 'email', width: 200, ellipsis: true },
    {
      title: '状态',
      dataIndex: 'status',
      search: false,
      width: 100,
      render: (_, row) =>
        row.status === 'active' ? (
          <Tag color="success">启用</Tag>
        ) : (
          <Tag color="default">禁用</Tag>
        ),
    },
    {
      title: '创建时间',
      dataIndex: 'createdAt',
      valueType: 'dateTime',
      search: false,
      width: 160,
    },
    {
      title: '操作',
      valueType: 'option',
      width: 220,
      fixed: 'right',
      render: (_, row) => (
        <Space>
          {access.canWriteBindings && (
            <Button
              type="link"
              size="small"
              icon={<SafetyCertificateOutlined />}
              onClick={() => openRoleModal(row)}
            >
              分配角色
            </Button>
          )}
          {access.can('admin:users:write') &&
            (row.status === 'active' ? (
              <Popconfirm
                title="确认禁用该账号？"
                onConfirm={async () => {
                  await updateAdminUserStatus(row.id, 'disabled');
                  message.success('已禁用');
                  actionRef.current?.reload();
                }}
              >
                <Button type="link" size="small" danger icon={<StopOutlined />}>
                  禁用
                </Button>
              </Popconfirm>
            ) : (
              <Popconfirm
                title="确认启用该账号？"
                onConfirm={async () => {
                  await updateAdminUserStatus(row.id, 'active');
                  message.success('已启用');
                  actionRef.current?.reload();
                }}
              >
                <Button type="link" size="small" icon={<CheckCircleOutlined />}>
                  启用
                </Button>
              </Popconfirm>
            ))}
        </Space>
      ),
    },
  ];

  const transferDataSource: TransferProps['dataSource'] = allRoles.map((r) => ({
    key: r.id,
    title: r.name,
    description: r.description,
  }));

  return (
    <PageContainer title="管理员用户">
      <ProTable<AdminUser>
        actionRef={actionRef}
        rowKey="id"
        columns={columns}
        scroll={{ x: 1080 }}
        request={async () => {
          const data = await listAdminUsers();
          return { data, success: true };
        }}
        search={{ labelWidth: 'auto' }}
        toolBarRender={() => [
          access.can('admin:users:write') && (
            <Button
              key="add"
              type="primary"
              icon={<PlusOutlined />}
              onClick={() => setCreateOpen(true)}
            >
              新建管理员
            </Button>
          ),
        ]}
      />

      {/* 新建管理员 */}
      <ModalForm<{ username: string; password: string; name?: string; email?: string }>
        title="新建管理员"
        open={createOpen}
        onOpenChange={setCreateOpen}
        onFinish={async (values) => {
          await createAdminUser(values);
          message.success('已创建');
          actionRef.current?.reload();
          return true;
        }}
      >
        <ProFormText
          name="username"
          label="用户名"
          rules={[{ required: true, message: '请输入用户名' }]}
        />
        <ProFormText.Password
          name="password"
          label="初始密码"
          rules={[{ required: true, message: '请输入初始密码' }]}
        />
        <ProFormText name="name" label="姓名" />
        <ProFormText name="email" label="邮箱" />
      </ModalForm>

      {/* 分配角色 */}
      <Modal
        title={`为「${roleTarget?.name || roleTarget?.username}」分配角色`}
        open={roleModal}
        onCancel={() => setRoleModal(false)}
        width={640}
        onOk={async () => {
          if (!roleTarget) return;
          await assignRolesToUser(roleTarget.id, targetKeys);
          message.success('角色已更新');
          setRoleModal(false);
        }}
      >
        <Transfer
          dataSource={transferDataSource}
          titles={['可选角色', '已分配']}
          targetKeys={targetKeys}
          onChange={(keys) => setTargetKeys(keys as string[])}
          render={(item) => item.title ?? ''}
          listStyle={{ width: 260, height: 320 }}
          showSearch
        />
      </Modal>
    </PageContainer>
  );
};

export default AdminUsersPage;
