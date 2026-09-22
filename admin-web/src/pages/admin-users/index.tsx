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
import { FULL_PAGE_PARAMS, toPageParams } from '../../services/pagination';
import { requestErrorMessage } from '../../services/requestError';

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
    // 候选角色要全集：分页后只给 Transfer 第 1 页会静默少几项可选项。
    const [roles, userRoles] = await Promise.all([
      listRoles(FULL_PAGE_PARAMS),
      listUserRoles(user.id),
    ]);
    setAllRoles(roles.items);
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
          {access.can('admin:users:manage') &&
            (row.status === 'active' ? (
              <Popconfirm
                title="确认禁用该账号？"
                onConfirm={async () => {
                  try {
                    await updateAdminUserStatus(row.id, 'disabled');
                    message.success('已禁用');
                    actionRef.current?.reload();
                  } catch (error) {
                    message.error(requestErrorMessage(error, '禁用失败，请稍后重试'));
                  }
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
                  try {
                    await updateAdminUserStatus(row.id, 'active');
                    message.success('已启用');
                    actionRef.current?.reload();
                  } catch (error) {
                    message.error(requestErrorMessage(error, '启用失败，请稍后重试'));
                  }
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
        request={async (params) => {
          const result = await listAdminUsers(toPageParams(params));
          return { data: result.items, total: result.total, success: true };
        }}
        // 没有搜索栏：`GET /v1/admin/users` 只解析 page / pageSize
        // （user-service internal/handler/user.go 的 List 里只有 ParsePage），用户名 / 姓名 /
        // 邮箱三个搜索框填了也发不出去、后端一个都不认，结果只会是「筛了跟没筛一样」。
        // 仓库里同一条取舍见 payments/index.tsx 的「渠道 / 支付方式」两列：不给搜索就不要
        // 摆那个框。等后端支持按这三个字段筛，再把 search 打开。
        search={false}
        toolBarRender={() => [
          access.can('admin:users:manage') && (
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
          try {
            await createAdminUser(values);
          } catch (error) {
            // 用户名重复一类的错只有后端知道，返回 false 让弹窗留在原地，
            // 填过的四个字段不丢。
            message.error(requestErrorMessage(error, '创建失败，请稍后重试'));
            return false;
          }
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
        <ProFormText
          name="name"
          label="姓名"
          rules={[{ required: true, message: '请输入姓名' }]}
        />
        <ProFormText
          name="email"
          label="邮箱"
          rules={[
            { required: true, message: '请输入邮箱' },
            { type: 'email', message: '邮箱格式不正确' },
          ]}
        />
      </ModalForm>

      {/* 分配角色 */}
      <Modal
        title={`为「${roleTarget?.name || roleTarget?.username}」分配角色`}
        open={roleModal}
        onCancel={() => setRoleModal(false)}
        width={640}
        // 不返回 Promise：失败时自行提示并保持弹窗打开，避免 antd 把已保存状态当成功
        // 关闭（与 roles 页两个分配弹窗同一写法）。角色清单是刚拉回来的，失败时留着重选
        // 比退出去再点一次「分配角色」强。
        onOk={async () => {
          if (!roleTarget) return;
          try {
            await assignRolesToUser(roleTarget.id, targetKeys);
          } catch (error) {
            message.error(requestErrorMessage(error, '角色保存失败，请稍后重试'));
            return;
          }
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
