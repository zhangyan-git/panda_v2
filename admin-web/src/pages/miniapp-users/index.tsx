import {
  CheckCircleOutlined,
  StopOutlined,
  UserOutlined,
} from '@ant-design/icons';
import {
  PageContainer,
  ProDescriptions,
  ProTable,
} from '@ant-design/pro-components';
import type { ActionType, ProColumns } from '@ant-design/pro-components';
import { useAccess } from '@umijs/max';
import { Avatar, Button, Drawer, message, Popconfirm, Space, Tag } from 'antd';
import { useRef, useState } from 'react';
import {
  getMiniappUser,
  listMiniappUsers,
  updateMiniappUserStatus,
  type MiniappLoginEvent,
  type MiniappUser,
  type MiniappUserDetail,
  type MiniappUserStatus,
  type MiniappWechatIdentity,
} from '../../services/miniappUser';
import { toPageParams } from '../../services/pagination';
import { requestErrorMessage } from '../../services/requestError';

/**
 * 状态展示。deleted 是用户自己注销后留下的终态，后台不能把他改回去——
 * 所以这一档既没有「启用」也没有「禁用」，见下方操作列。
 */
const statusMeta: Record<MiniappUserStatus, { text: string; color: string }> = {
  active: { text: '正常', color: 'success' },
  disabled: { text: '已禁用', color: 'default' },
  deleted: { text: '已注销', color: 'error' },
};

const registerSourceText: Record<string, string> = {
  wechat_miniapp: '微信一键登录',
  wechat_phone: '微信手机号',
  sms_code: '短信验证码',
};

const loginTypeText = registerSourceText;

// 原始值直接显示会是「unknown」——管理员看到的是给机器看的词。
const genderText: Record<MiniappUser['gender'], string> = {
  unknown: '未知',
  male: '男',
  female: '女',
};

export default function MiniappUsersPage() {
  const access = useAccess();
  const actionRef = useRef<ActionType>();
  const [detail, setDetail] = useState<MiniappUserDetail>();

  const canManage = access.canManageMiniappUsers;

  const openDetail = async (id: string) => {
    try {
      setDetail(await getMiniappUser(id));
    } catch (error) {
      message.error(requestErrorMessage(error, '加载详情失败'));
    }
  };

  const changeStatus = async (row: MiniappUser, status: 'active' | 'disabled') => {
    try {
      const { revokedSessions } = await updateMiniappUserStatus(row.id, status);
      // 把撤销的会话数报出来：管理员点「禁用」时真正关心的是「他现在还能不能用」，
      // 只说一句「成功」等于没回答。
      message.success(
        status === 'disabled'
          ? `已禁用${revokedSessions > 0 ? `，同时踢下线 ${revokedSessions} 个登录态` : ''}`
          : '已启用',
      );
      actionRef.current?.reload();
      // 详情抽屉开着时同步刷新：禁用会把 activeSessions 打到 0，
      // 不刷新的话抽屉里还显示着禁用前的会话数，和列表对不上。
      if (detail?.id === row.id) await openDetail(row.id);
    } catch (error) {
      message.error(requestErrorMessage(error, '操作失败，请稍后重试'));
    }
  };

  const columns: ProColumns<MiniappUser>[] = [
    {
      // 仅搜索用的字段：hideInTable 让它只出现在查询表单里。
      // 后端按「手机号前缀 或 昵称片段」匹配。
      title: '关键词',
      dataIndex: 'keyword',
      hideInTable: true,
      fieldProps: { placeholder: '手机号前缀或昵称' },
    },
    {
      title: '用户',
      dataIndex: 'nickname',
      search: false,
      width: 180,
      render: (_, row) => (
        <Space>
          <Avatar src={row.avatarUrl || undefined} icon={<UserOutlined />} size="small" />
          <span>{row.nickname || '（未设置昵称）'}</span>
        </Space>
      ),
    },
    {
      title: '手机号',
      dataIndex: 'phone',
      search: false,
      copyable: true,
      width: 140,
      render: (_, row) => row.phone || '—',
    },
    {
      title: '状态',
      dataIndex: 'status',
      valueType: 'select',
      width: 100,
      valueEnum: {
        active: { text: '正常' },
        disabled: { text: '已禁用' },
        deleted: { text: '已注销' },
      },
      render: (_, row) => {
        const meta = statusMeta[row.status] ?? { text: row.status, color: 'default' };
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      title: '注册来源',
      dataIndex: 'registerSource',
      search: false,
      width: 120,
      render: (_, row) => registerSourceText[row.registerSource] ?? row.registerSource,
    },
    {
      title: '最后登录',
      dataIndex: 'lastLoginAt',
      // 交给 ProTable 按 dateTime 渲染（本地时区）。本页别自己拼时间字符串：
      // 同一个表里两种格式（一个带 T 和 Z、一个不带）看着像两个系统拼出来的。
      // 从未登录时后端给空串，由下面 request 里的 normalize 转成 undefined。
      valueType: 'dateTime',
      search: false,
      width: 160,
    },
    {
      title: '登录次数',
      dataIndex: 'loginCount',
      search: false,
      width: 90,
    },
    {
      title: '注册时间',
      dataIndex: 'createdAt',
      valueType: 'dateTime',
      search: false,
      width: 160,
    },
    {
      title: '操作',
      valueType: 'option',
      fixed: 'right',
      width: 160,
      render: (_, row) => [
        <Button key="detail" type="link" size="small" onClick={() => openDetail(row.id)}>
          详情
        </Button>,
        // 已注销的账号不给任何状态操作：后端会以 409 拒绝，按钮摆在那里
        // 只会让管理员以为点了没反应。
        canManage && row.status !== 'deleted' ? (
          row.status === 'active' ? (
            <Popconfirm
              key="disable"
              title="确认禁用该用户？"
              description="禁用会同时撤销他当前所有登录态，他需要重新登录（且登不进来）。"
              onConfirm={() => changeStatus(row, 'disabled')}
            >
              <Button type="link" size="small" danger icon={<StopOutlined />}>
                禁用
              </Button>
            </Popconfirm>
          ) : (
            <Popconfirm
              key="enable"
              title="确认启用该用户？"
              onConfirm={() => changeStatus(row, 'active')}
            >
              <Button type="link" size="small" icon={<CheckCircleOutlined />}>
                启用
              </Button>
            </Popconfirm>
          )
        ) : null,
      ],
    },
  ];

  const wechatIdentityColumns: ProColumns<MiniappWechatIdentity>[] = [
    {
      title: '应用',
      dataIndex: 'appType',
      width: 110,
      render: (_, row) => (row.appType === 'miniapp' ? '小程序' : row.appType),
    },
    { title: 'openId', dataIndex: 'openId', copyable: true, ellipsis: true },
    {
      title: 'unionId',
      dataIndex: 'unionId',
      // 没绑定微信开放平台的小程序拿不到 unionid，这一列经常是空的。
      ellipsis: true,
      render: (_, row) => row.unionId || '（未绑定开放平台）',
    },
    { title: '最近登录', dataIndex: 'lastLoginAt', valueType: 'dateTime', width: 170 },
  ];

  const loginEventColumns: ProColumns<MiniappLoginEvent>[] = [
    {
      title: '时间',
      dataIndex: 'createdAt',
      valueType: 'dateTime',
      width: 170,
    },
    {
      title: '方式',
      dataIndex: 'loginType',
      width: 110,
      render: (_, row) => loginTypeText[row.loginType] ?? row.loginType,
    },
    {
      // 失败时这一列是手机号（已脱敏）或 openid，成功时也是同一个值——
      // 它就是这次尝试用的那个凭据标识。
      title: '标识',
      dataIndex: 'identifier',
      ellipsis: true,
    },
    {
      title: '结果',
      dataIndex: 'success',
      width: 140,
      render: (_, row) =>
        row.success ? (
          <Tag color="success">成功</Tag>
        ) : (
          <Tag color="error">{row.failReason || '失败'}</Tag>
        ),
    },
  ];

  return (
    <PageContainer title="小程序用户">
      <ProTable<MiniappUser>
        actionRef={actionRef}
        rowKey="id"
        columns={columns}
        scroll={{ x: 1180 }}
        // 默认按注册时间倒序（后端排的）：后台看的是最近注册的人，
        // 翻页也该从近往远走。
        request={async (params) => {
          const result = await listMiniappUsers(toPageParams(params));
          return { data: result.items, total: result.total, success: true };
        }}
        search={{ labelWidth: 'auto' }}
      />

      <Drawer
        title="用户详情"
        width={720}
        open={!!detail}
        onClose={() => setDetail(undefined)}
      >
        {detail && (
          <>
            <ProDescriptions<MiniappUserDetail>
              column={2}
              dataSource={detail}
              columns={[
                { title: '用户 ID', dataIndex: 'id', copyable: true },
                { title: '手机号', dataIndex: 'phone', copyable: true, render: (_, row) => row.phone || '—' },
                { title: '昵称', dataIndex: 'nickname', render: (_, row) => row.nickname || '—' },
                {
                  title: '状态',
                  dataIndex: 'status',
                  render: (_, row) => {
                    const meta = statusMeta[row.status] ?? { text: row.status, color: 'default' };
                    return <Tag color={meta.color}>{meta.text}</Tag>;
                  },
                },
                {
                  title: '注册来源',
                  dataIndex: 'registerSource',
                  render: (_, row) => registerSourceText[row.registerSource] ?? row.registerSource,
                },
                {
                  title: '性别',
                  dataIndex: 'gender',
                  render: (_, row) => genderText[row.gender] ?? row.gender,
                },
                { title: '生日', dataIndex: 'birthday', render: (_, row) => row.birthday || '—' },
                {
                  title: '地区',
                  dataIndex: 'regionName',
                  render: (_, row) => row.regionName || row.regionCode || '—',
                },
                {
                  title: '有效会话数',
                  dataIndex: 'activeSessions',
                  // 这个数字是「他现在还能不能继续用」的直接答案：禁用后归零。
                  // 为 0 说明手里的令牌已经换不出新的了。
                  render: (_, row) => (
                    <span>{row.activeSessions > 0 ? row.activeSessions : '0（无活跃登录态）'}</span>
                  ),
                },
                { title: '最后登录', dataIndex: 'lastLoginAt', valueType: 'dateTime' },
                { title: '最后登录 IP', dataIndex: 'lastLoginIp', render: (_, row) => row.lastLoginIp || '—' },
                { title: '登录次数', dataIndex: 'loginCount' },
                { title: '注册时间', dataIndex: 'createdAt', valueType: 'dateTime' },
              ]}
            />

            <ProTable<MiniappWechatIdentity>
              headerTitle="微信绑定"
              style={{ marginTop: 24 }}
              rowKey="openId"
              columns={wechatIdentityColumns}
              dataSource={detail.wechatIdentities}
              search={false}
              pagination={false}
              options={false}
              size="small"
            />

            <ProTable<MiniappLoginEvent>
              headerTitle="最近登录记录"
              style={{ marginTop: 24 }}
              rowKey={(row) => `${row.createdAt}-${row.loginType}-${row.identifier}`}
              columns={loginEventColumns}
              dataSource={detail.recentLogins}
              search={false}
              pagination={false}
              options={false}
              size="small"
            />
          </>
        )}
      </Drawer>
    </PageContainer>
  );
}
