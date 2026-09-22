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
  /** 正在取「这个人已有哪些角色」的那一行，见 openRoleModal。 */
  const [roleLoadingId, setRoleLoadingId] = useState<string>();

  /**
   * 取「已分配角色」的请求序号。
   *
   * 打开弹窗要先取一次这个人当前的角色，而这件事是**并发**的：点了 A 的「分配角色」，链接还没
   * 回来又去点 B。先发出去的那一次可能后回来，于是**弹窗标题写着 B、勾选的却是 A 的角色**——
   * 点「确定」就是把 A 的角色整体覆盖到 B 身上（后端是整体覆盖语义，见 assignRolesToUser）。
   * 这是一次**权限写入**，错的不是显示而是 B 从此多/少了一批权限。
   *
   * 所以每次发请求领一个号，只有**最后领号的那一次**的结果才落到状态上；被取代的那一次连错误
   * 都不报（那一下已经被后来的动作取代了，报一句只会让人以为刚才点失败了）。与
   * payments/settlement-rules 的 detailSeq 同一个写法。
   */
  const roleSeq = useRef(0);

  const openRoleModal = async (user: AdminUser) => {
    const seq = ++roleSeq.current;
    setRoleLoadingId(user.id);
    try {
      // 候选角色要全集：分页后只给 Transfer 第 1 页会静默少几项可选项。
      const [roles, userRoles] = await Promise.all([
        listRoles(FULL_PAGE_PARAMS),
        listUserRoles(user.id),
      ]);
      // 落状态之前再看一眼序号：这中间点了别的一行的话，这一次拿到的就是过期数据。
      if (seq !== roleSeq.current) return;
      // 四份状态**同一批**落下去，标题（roleTarget）与勾选（targetKeys）必须来自同一次请求。
      setRoleTarget(user);
      setAllRoles(roles.items);
      setTargetKeys(userRoles.map((r) => r.id));
      setRoleModal(true);
    } catch (error) {
      if (seq !== roleSeq.current) return;
      message.error(requestErrorMessage(error, '加载角色失败，请稍后重试'));
    } finally {
      // 只有最后那一次负责把转圈停掉——先发的那次回来时，按钮上转的是后发的那一行。
      if (seq === roleSeq.current) setRoleLoadingId(undefined);
    }
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
              // 转圈是为了让人知道「点了，在等」：详情要一次往返，点在没反应的那几百毫秒里，
              // 人会以为按钮坏了——而这里再点一下就是上面那个并发问题。
              loading={roleLoadingId === row.id}
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
