import {
  CheckCircleOutlined,
  StopOutlined,
  UserOutlined,
  WalletOutlined,
} from '@ant-design/icons';
import {
  ModalForm,
  PageContainer,
  ProDescriptions,
  ProFormDependency,
  ProFormDateTimePicker,
  ProFormDigit,
  ProFormSelect,
  ProFormTextArea,
  ProTable,
} from '@ant-design/pro-components';
import type { ActionType, ProColumns } from '@ant-design/pro-components';
import { history, useAccess } from '@umijs/max';
import { Avatar, Button, Drawer, message, Popconfirm, Space, Tag } from 'antd';
import { useRef, useState } from 'react';
import {
  adjustCoffeeBeans,
  getCoffeeBeanAccount,
  listCoffeeBeanEntries,
  type CoffeeBeanAccount,
  type CoffeeBeanEntry,
  type CoffeeBeanEntryType,
} from '../../services/coffeeBean';
import { formatDateTime, toRFC3339 } from '../../services/datetime';
import { enumMeta } from '../../services/labels';
import {
  grantMembership,
  listMembershipPlans,
  listMemberships,
  type Membership,
  type MembershipPlan,
} from '../../services/membership';
import { MEMBERSHIP_STATUS } from '../../services/membershipLabels';
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
import { fenToYuan, formatSignedYuan, formatYuan, yuanToFen } from '../../services/money';
import { FULL_PAGE_PARAMS, toPageParams } from '../../services/pagination';
import { requestErrorMessage } from '../../services/requestError';
import { listStores, type Store } from '../../services/store';

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

/**
 * 咖啡豆账变的类型文案。取值来自 account-service 的 model 常量（BeanEntryType*），
 * 与 migrations/account/005 的 CHECK 一致。
 *
 * 文案用「人工调整 / 订单扣减 / 退款冲正」而不是「收入 / 支出」：类型回答的是**为什么**变，
 * 方向已经由金额的符号回答了。写成收支会让人分不清一次冲正到底加还是减。
 */
const beanEntryTypeMeta: Record<CoffeeBeanEntryType, { text: string; color: string }> = {
  adjust: { text: '人工调整', color: 'gold' },
  consume: { text: '订单扣减', color: 'blue' },
  reverse: { text: '退款冲正', color: 'warning' },
};

/**
 * 咖啡豆的金额：**接口给的是「分」，这一页一律按「元」展示与录入**——与订单、优惠券、
 * 设备余额同一条约定（见 services/money.ts 开头那段）。
 *
 * 换算只发生在本文件的边界上：读出来的分过一遍 formatYuan，填进去的元过一遍 yuanToFen，
 * 中间不再出现第二种单位。
 */

/** axios 错误里的 HTTP 状态码，没有响应（断网、超时）时是 undefined。 */
const statusOf = (error: unknown) =>
  (error as { response?: { status?: number } } | null)?.response?.status;

type BeanAdjustFormValues = {
  amount?: number;
  remark?: string;
};

export default function MiniappUsersPage() {
  const access = useAccess();
  const actionRef = useRef<ActionType>();
  const [detail, setDetail] = useState<MiniappUserDetail>();
  const beanEntriesRef = useRef<ActionType>();

  /**
   * 咖啡豆账户。三态而不是两态：
   *   undefined = 还没拉或正在拉，界面写「读取中…」；
   *   null      = 拉了但失败（多半是没有 account:read），界面写「读不到余额」；
   *   对象      = 拉到了。
   * 少了 null 这一档，读失败就会永远停在「读取中…」，看起来像卡住了。
   */
  const [beanAccount, setBeanAccount] = useState<CoffeeBeanAccount | null>();

  /**
   * 会员资格（membership-service）。比咖啡豆**多一档**，四态：
   *   undefined = 还没拉或正在拉，界面写「读取中…」；
   *   null      = 拉了但失败（多半是没有 membership:read）；
   *   'none'    = 拉到了，这个人**确实没有**会员资格；
   *   对象      = 拉到了。
   *
   * 三态不够用正是因为这个 'none'：客服点开一个人最先问的就是「他是不是会员」，
   * 而「不是会员」与「读不到」在界面上必须是两句不同的话——混成一个样子，前者的
   * 答案是「不是」还是「不知道」就分不出来了。
   */
  const [membership, setMembership] = useState<Membership | 'none' | null>();

  // 余额调整
  const [beanAdjustOpen, setBeanAdjustOpen] = useState(false);
  // 幂等键，跟着「打开弹窗」走：同一个 requestId 重发只会记一次账，所以它必须在用户改金额
  // 时保持不动，只在这一轮调整结束时才换。
  const [beanRequestId, setBeanRequestId] = useState('');

  // 开通会员。幂等键的规矩与余额调整一样：跟着「打开弹窗」走一次，重发沿用它。
  const [grantOpen, setGrantOpen] = useState(false);
  const [grantRequestId, setGrantRequestId] = useState('');

  const canManage = access.canManageMiniappUsers;

  /**
   * 重新取一次这个人的咖啡豆账户。调整成功、或 409 之后要把真实余额摆出来。
   *
   * 失败回 null（而不是抛）：409 那条路上调用方正要提示「请核对余额」，这里再抛一个错
   * 只会盖住那句话。返回最新值也让调用方不必再读一遍 state（它是异步的）。
   */
  const refreshBeanAccount = async (id: string): Promise<CoffeeBeanAccount | null> => {
    try {
      const latest = await getCoffeeBeanAccount(id);
      setBeanAccount(latest);
      return latest;
    } catch {
      setBeanAccount(null);
      return null;
    }
  };

  /**
   * 重新取一次这个人的会员资格。
   *
   * 按 userId 精确筛、只取一条：`memberships_user_unique` 保证一个用户至多一条会员记录，
   * 所以「取第一条」不是「随便挑一条」。
   */
  const refreshMembership = async (id: string) => {
    try {
      const page = await listMemberships({ userId: id, page: 1, pageSize: 1 });
      setMembership(page.items[0] ?? 'none');
    } catch {
      setMembership(null);
    }
  };

  const openDetail = async (id: string) => {
    try {
      setDetail(await getMiniappUser(id));
    } catch (error) {
      message.error(requestErrorMessage(error, '加载详情失败'));
      return;
    }
    // 余额与会员各自单独拉、单独失败：它们是另外两个上游（account-service / membership-service）
    // 的两件事，读不到只是那一个区块显示「读不到」，不该让整个抽屉打不开——没有那两枚读权限的
    // 账号照样要能看用户。先把两处都置回「读取中」，否则会拿上一个人的值糊在这一档上。
    setBeanAccount(undefined);
    setMembership(undefined);
    await Promise.all([refreshBeanAccount(id), refreshMembership(id)]);
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

  const beanEntryColumns: ProColumns<CoffeeBeanEntry>[] = [
    {
      title: '时间',
      dataIndex: 'occurredAt',
      // 业务发生时间（扣减是支付时间、冲正是审核时间），不是这一行落库的时间。
      valueType: 'dateTime',
      width: 150,
    },
    {
      title: '类型',
      dataIndex: 'entryType',
      width: 90,
      render: (_, row) => {
        const meta = beanEntryTypeMeta[row.entryType];
        return meta ? <Tag color={meta.color}>{meta.text}</Tag> : row.entryType;
      },
    },
    {
      // 带符号（+¥12.34 / -¥0.10）：这个数是有符号的（充值为正、扣减为负），只摆绝对值
      // 会让一次扣减看起来像又进了一笔钱。
      title: '金额（元）',
      dataIndex: 'amount',
      width: 110,
      render: (_, row) => (
        <span style={{ color: row.amount > 0 ? '#3f8600' : '#cf1322' }}>
          {formatSignedYuan(row.amount)}
        </span>
      ),
    },
    {
      // 「变动后」而不是当前余额：冲正重放时只有它能回答「当时是多少」。
      title: '变动后（元）',
      dataIndex: 'balanceAfter',
      width: 110,
      render: (_, row) => `¥${formatYuan(row.balanceAfter)}`,
    },
    {
      // 后端给的那句话（「后台充值」「下单支付」…），下面是操作人填的理由——调整那一条
      // 的 remark 就在这儿，它是「为什么加这笔钱」唯一的界面留痕。
      title: '说明',
      dataIndex: 'title',
      render: (_, row) => (
        <>
          <div>{row.title}</div>
          {row.remark && (
            <div style={{ color: '#8c8c8c', fontSize: 12 }}>{row.remark}</div>
          )}
        </>
      ),
    },
    {
      // 三种形状：调整是幂等号、扣减是订单号、冲正是售后单号。所以列的标题不带「订单」。
      title: '关联单号',
      dataIndex: 'referenceNo',
      ellipsis: true,
      render: (_, row) => row.referenceNo || '—',
    },
  ];

  // 会员那一块要的三态里有两态不是对象（'none' / null），所以先把「真有会员」这一档
  // 收成一个对象再渲染，免得每处都写一遍 `membership !== 'none'` 的收窄。
  const currentMembership = typeof membership === 'object' && membership !== null ? membership : null;

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

            {/* 会员资格（membership-service）。与下面那块同理：它不是用户服务给的，
                `detail` 里没有，要另发一次请求。会员库不 join 用户服务（跨库），所以
                这里只拿得到一个 userId、一条资格，昵称手机号还是上面那份 detail。

                **这里能开会员**（2026-09 起）：本域的设计是「会员在别处成交、在这里生效」，
                唯一的例外就是这一颗按钮——客服补偿、线下活动、渠道争议都没有订单，不给这个
                入口就没有任何补救路径。它是 membership:adjust（**直接白送钱**那一枚），
                不是 manage，所以只有拿得到那枚权限的人才看得见。

                已经有会员的人**不显示它**：后端遇到已有会员一律 409，不叠加续期。要给已经
                过期的人恢复权益，走会员详情页的「调整有效期」——那本来就是干这个的。 */}
            <div
              style={{
                marginTop: 24,
                display: 'flex',
                alignItems: 'center',
                justifyContent: 'space-between',
                gap: 12,
              }}
            >
              <Space size={8}>
                <strong>会员</strong>
                {membership === undefined && <span style={{ color: '#8c8c8c' }}>读取中…</span>}
                {membership === null && (
                  <span style={{ color: '#8c8c8c' }}>读不到会员资格（可能没有查看权限）</span>
                )}
                {membership === 'none' && <Tag>非会员</Tag>}
                {currentMembership && (
                  <>
                    <Tag color={enumMeta(MEMBERSHIP_STATUS, currentMembership.status).color}>
                      {enumMeta(MEMBERSHIP_STATUS, currentMembership.status).text}
                    </Tag>
                    <span>{currentMembership.planName}</span>
                    <span style={{ color: '#8c8c8c' }}>
                      {/* 「有效至」还是「到期于」看接口算好的 active，不重算：
                          到期扫描没跑完的那段时间里 status 还是 active 而 expireAt 已经过了。 */}
                      {currentMembership.active ? '有效至' : '到期于'}{' '}
                      {formatDateTime(currentMembership.expireAt)}
                    </span>
                    {/* 归属门店：「这个人算哪家店的业绩」，不参与任何金额计算。空串是没有归属，
                        不是错误——而名字是后端现解的，解不出来时也是空串（这时候退回显示 id，
                        至少还能对上账）。 */}
                    <span style={{ color: '#8c8c8c' }}>
                      归属门店：
                      {currentMembership.storeName || currentMembership.storeId || '—'}
                    </span>
                  </>
                )}
              </Space>
              <Space size={8}>
                {currentMembership && (
                  <Button onClick={() => history.push(`/membership/members/${currentMembership.id}`)}>
                    查看会员详情
                  </Button>
                )}
                {/* 只在「确实不是会员」这一档出现，而且只在能 adjust 的人手里出现——见上面那段注释。 */}
                {membership === 'none' && access.canAdjustMembership && (
                  <Button
                    type="primary"
                    onClick={() => {
                      // 每开一次弹窗就是一次新的开通意图，配一枚新的幂等键。重发（网络抖动、
                      // 手滑连点）沿用同一枚，后端据此认出「这是同一次点击」而不是「又来开一个」。
                      setGrantRequestId(crypto.randomUUID());
                      setGrantOpen(true);
                    }}
                  >
                    开通会员
                  </Button>
                )}
              </Space>
            </div>

            {/* 咖啡豆账户（account-service 的用户维度余额，接口回的是分、这里按元显示）。
                与上面两块不同，它不是用户服务给的：`detail` 里没有余额，要另发一次请求。
                所以这里既可能「还没拉到」，也可能「拉不到」（没有 account:read 时），
                两种状态分开写，别把读失败显示成 ¥0.00——那会让人以为用户真的一分没有。 */}
            <div
              style={{
                marginTop: 24,
                display: 'flex',
                alignItems: 'center',
                justifyContent: 'space-between',
                gap: 12,
              }}
            >
              <Space size={8}>
                <strong>咖啡豆账户</strong>
                {beanAccount === undefined && <span style={{ color: '#8c8c8c' }}>读取中…</span>}
                {beanAccount === null && (
                  <span style={{ color: '#8c8c8c' }}>读不到余额（可能没有查看权限）</span>
                )}
                {beanAccount && (
                  <span>
                    余额：<strong>¥{formatYuan(beanAccount.balance)}</strong>
                    {!beanAccount.hasAccount && (
                      <span style={{ color: '#8c8c8c' }}>（从未有过账户）</span>
                    )}
                  </span>
                )}
              </Space>
              {access.canAdjustCoffeeBeans && (
                <Button
                  icon={<WalletOutlined />}
                  onClick={() => {
                    // 每开一次弹窗就是一次新的调整意图，配一枚新的幂等键。
                    setBeanRequestId(crypto.randomUUID());
                    setBeanAdjustOpen(true);
                  }}
                >
                  余额调整
                </Button>
              )}
            </div>

            <ProTable<CoffeeBeanEntry>
              actionRef={beanEntriesRef}
              headerTitle="余额流水"
              style={{ marginTop: 8 }}
              rowKey="id"
              columns={beanEntryColumns}
              // params 而不是在 request 里闭包 detail.id：抽屉没关就点了另一个用户时，
              // 组件不重新挂载，只有 params 变了才会重新发请求。写错的话会显示上一个人的流水。
              params={{ userId: detail.id }}
              request={async (params) => {
                const result = await listCoffeeBeanEntries({
                  ...toPageParams(params),
                  userId: detail.id,
                });
                return { data: result.items, total: result.total, success: true };
              }}
              search={false}
              options={false}
              size="small"
              pagination={{ pageSize: 5, size: 'small', showSizeChanger: false }}
            />
          </>
        )}
      </Drawer>

      {/* 余额调整。**表单按元填**、金额**带符号**（充值为正、纠错为负），没有「方向」那一栏：
          后台一律按元录入（与优惠券面额、饮品价格同一条约定），发出去之前过一遍 yuanToFen
          换成分——符号本来就由金额自己带着，再让方向决定一次符号只会多一处错位的机会。 */}
      <ModalForm<BeanAdjustFormValues>
        // 换一个人换一个 key，关上再开靠 destroyOnClose 清字段。缺了这两个，改一个人之后再改
        // 另一个，上一行填的金额会跟着过去——而这是个「加钱」的弹窗。
        key={`beans-${detail?.id ?? ''}`}
        title={`调整咖啡豆余额「${detail?.nickname || detail?.phone || detail?.id || ''}」`}
        open={beanAdjustOpen}
        onOpenChange={setBeanAdjustOpen}
        modalProps={{ destroyOnClose: true }}
        onFinish={async (values) => {
          if (!detail) return false;
          // 表单里是元，发出去的是分。判断放在换算之后：`0` 与「换成分不足 1 分」的
          // 0.001 元都会落到 amount === 0 上，而这两种后端都只回一句「金额不能为 0」，
          // 说不清是被四舍五入吃掉了。
          const amount = yuanToFen(values.amount);
          if (amount === 0) {
            message.error('调整金额不能为 0，最少 0.01 元');
            return false;
          }
          try {
            const result = await adjustCoffeeBeans(detail.id, {
              amount,
              requestId: beanRequestId,
              remark: values.remark,
            });
            message.success(`已调整，当前余额 ¥${formatYuan(result.balance)}`);
            // 用返回的余额直接改对，不再多读一次：那个数就是刚写完这笔之后的余额，
            // 重新读一次拿到的可能是别人又动过的。
            setBeanAccount((prev) => ({
              userId: detail.id,
              balance: result.balance,
              hasAccount: true,
              createdAt: prev?.createdAt ?? '',
              updatedAt: prev?.updatedAt ?? '',
            }));
            beanEntriesRef.current?.reload();
            return true;
          } catch (error) {
            if (statusOf(error) === 409) {
              // 后端把「同一个 requestId 又来了」回成 409 而不是装作成功：上一次多半已经
              // 记过账，只是响应没收到。到底记没记只有流水说得清，接口不替调用方下结论。
              // 所以这里既不报成功也不报失败，把最新余额摆出来让人自己核。
              const latest = await refreshBeanAccount(detail.id);
              beanEntriesRef.current?.reload();
              // 这枚 requestId 已经用掉了，留着它下次提交还是 409。换一枚，让「再调一次」
              // 真的是新的一次调整，而不是又撞上同一次的账。
              setBeanRequestId(crypto.randomUUID());
              message.warning(
                latest
                  ? `这次调整的流水已经记过账（上一次多半已经成功），当前余额 ¥${formatYuan(
                      latest.balance,
                    )}，请核对`
                  : '这次调整的流水已经记过账（上一次多半已经成功），请核对余额后再决定要不要重发',
              );
              return false;
            }
            // 网络错误这类「不知道成没成」的情况：弹窗不关、requestId 不换，重发还是同一次
            // 调整，靠它幂等。
            message.error(requestErrorMessage(error, '调整失败，请稍后重试'));
            return false;
          }
        }}
      >
        <ProFormDependency name={['amount']}>
          {({ amount }) => {
            // 余额读不到时（没有 account:read）就不摆这一行：摆一个 0 会让人以为用户没钱，
            // 而这里唯一能给的是「调整后是多少」这个减法，底数不对，算出来的数就是错的。
            if (!beanAccount) return null;
            // 后端把「扣成负数」回成 400，但那是点完保存才知道的。这里先算给自己看：
            // 一个把余额填成负数的输入，多半是少打了一位或符号填反了。
            //
            // 这一行**双方都是元**：余额是库里的分换算过来的，填的数本来就是元，所以直接相加。
            const after = fenToYuan(beanAccount.balance) + Number(amount ?? 0);
            return (
              <div style={{ marginBottom: 16 }}>
                <Space size="large">
                  <span>
                    当前余额：<strong>¥{formatYuan(beanAccount.balance)}</strong>
                  </span>
                  <span>
                    调整后：
                    <strong style={{ color: after < 0 ? '#cf1322' : undefined }}>
                      ¥{after.toFixed(2)}
                    </strong>
                  </span>
                </Space>
                {after < 0 && (
                  <div style={{ color: '#cf1322', marginTop: 4 }}>
                    余额不能为负，请调整金额
                  </div>
                )}
              </div>
            );
          }}
        </ProFormDependency>
        <ProFormDigit
          name="amount"
          label="调整金额"
          // min 是给手用的，不是业务规则：后端唯一的硬规矩是「不为 0」，余额扣穿了由
          // coffee_bean_accounts 的 CHECK 挡。设一个负的下限有两个用处——让输入框允许
          // 打 '-' 号，以及拦住多打几位 0 的手滑。真需要调这么大时改这一个数。
          // 上下限是**元**（±100 万元，与换算前的 ±1 亿分是同一个量级）。
          min={-1000000}
          max={1000000}
          // precision 限死两位小数：再细就不到 1 分，yuanToFen 会把它四舍五入掉，
          // 而界面上看起来像是「照着填的」。
          fieldProps={{ precision: 2, step: 0.01, style: { width: '100%' } }}
          extra="单位：元，可填负数（充值为正，把充错的豆调回来为负）"
          rules={[{ required: true, message: '请输入调整金额' }]}
        />
        <ProFormTextArea
          name="remark"
          label="备注"
          tooltip="余额调整全程留痕，备注会跟着这次操作一起记进流水"
          placeholder="例如：活动补偿 / 充错了调回"
        />
      </ModalForm>

      {/* 开通会员。**会员域唯一的创建入口**——别的会员都是「在别处成交、在这里生效」，
          只有这一条没有订单（客服补偿、线下活动、渠道争议）。后端会写一条平台审计。 */}
      <ModalForm<{
        planId: string;
        storeId?: string;
        expireAt?: unknown;
        reason: string;
        remark?: string;
      }>
        // key 与 destroyOnClose 缺一不可：换一个人再开，上一轮选的套餐 / 门店 / 日期会跟着
        // 过去，而这是个**直接送钱**的弹窗。
        key={`grant-${detail?.id ?? ''}`}
        title={`给「${detail?.nickname || detail?.phone || detail?.id || ''}」开通会员`}
        open={grantOpen}
        onOpenChange={setGrantOpen}
        modalProps={{ destroyOnClose: true, maskClosable: false }}
        onFinish={async (values) => {
          if (!detail) return false;
          const expireAt = toRFC3339(values.expireAt);
          try {
            const granted = await grantMembership({
              userId: detail.id,
              planId: values.planId,
              expireAt,
              // 空串按「没选」发：后端把空串当没有归属，而空串与 undefined 在请求体里
              // 确实是两回事（一个进了 JSON，一个没进）。
              storeId: values.storeId || undefined,
              reason: values.reason.trim(),
              remark: values.remark?.trim() || undefined,
              requestId: grantRequestId,
            });
            message.success(`已开通，有效期至 ${formatDateTime(granted.expireAt)}`);
            // 重取一次而不是把返回值摆上去：这里要的是列表那一档的形状，而不多读一次的
            // 收益只有几十毫秒。
            await refreshMembership(detail.id);
            return true;
          } catch (error) {
            if (statusOf(error) === 409) {
              // 已经有会员了。**不能重试**：后端不叠加续期（一个人一条会员），而重试同一次
              // 提交不会走到这里——那一条由 requestId 认出来是重放，回的是 200。
              // 所以到这一档只有一种解释：这个人现在确实有会员了（多半是别人刚开过，
              // 或者抽屉里那一档读的时候还没读到）。把真实状态摆出来让人自己看。
              message.warning(requestErrorMessage(error, '这个人已经是会员了，不能重复开通'));
              await refreshMembership(detail.id);
              return true;
            }
            // 网络错误这类「不知道成没成」：弹窗不关、requestId 不换，重发还是同一次开通。
            message.error(requestErrorMessage(error, '开通失败，请稍后重试'));
            return false;
          }
        }}
      >
        <p>
          这是给一个**还不是会员**的人直接开一条会员，不产生订单、不产生支付。到期之前不能续期
          ——要延长已经开出去的会员，去会员详情页用「调整有效期」。
        </p>
        <ProFormSelect
          name="planId"
          label="套餐"
          width="md"
          // 只列在售（active）的：后端对草稿与已下架一律 409（「这个套餐现在不能卖」）。
          // request 在弹窗每次打开时跑一次——destroyOnClose 会把内容卸掉，所以拿到的不是
          // 上一次打开时的缓存。
          request={async () => {
            const page = await listMembershipPlans({ ...FULL_PAGE_PARAMS, status: 'active' });
            return page.items.map((plan: MembershipPlan) => ({
              label: `${plan.name}（¥${formatYuan(plan.priceCents)} / ${
                plan.period === 'month' ? `${plan.periodCount} 个月` : `${plan.periodCount} 年`
              }）`,
              value: plan.id,
            }));
          }}
          fieldProps={{ showSearch: true, optionFilterProp: 'label' }}
          rules={[{ required: true, message: '请选择套餐' }]}
        />
        <ProFormSelect
          name="storeId"
          label="归属门店"
          width="md"
          tooltip="这个人算哪家店的业绩。与会员权益无关——会员价在哪家店用都一样，也不参与分账"
          // 可以留空：线上来的会员没有归属门店。后端会先问一次「这家店在不在」，
          // 不在就拒（不会留下一条查无此店的会员）。
          request={async () => {
            const page = await listStores(FULL_PAGE_PARAMS);
            return page.items.map((store: Store) => ({
              label: `${store.name}（${store.brandName || store.merchantName}）`,
              value: store.id,
            }));
          }}
          fieldProps={{ showSearch: true, optionFilterProp: 'label', allowClear: true }}
        />
        <ProFormDateTimePicker
          name="expireAt"
          label="到期时间"
          width="md"
          fieldProps={{ format: 'YYYY-MM-DD HH:mm:ss' }}
          // **留空就是套餐自带的时长**（后端按 period / periodCount 从当下算），要「送到年底」
          // 这类补偿时才在这里指定。不预填一个日期：那需要在浏览器里把后端的日历加法再实现
          // 一遍（包月加一个月这条规则有两个实现就迟早会走偏），而留空本来就等价于那个默认值。
          extra="留空 = 按所选套餐的时长自动计算"
        />
        <ProFormTextArea
          name="reason"
          label="原因"
          fieldProps={{
            rows: 3,
            maxLength: 200,
            showCount: true,
            placeholder: '为什么开通（必填）。比如「店长答应的补偿，工单 12345」',
          }}
          rules={[{ required: true, message: '请填原因' }]}
        />
        <ProFormTextArea
          name="remark"
          label="备注"
          fieldProps={{ rows: 2, maxLength: 500, showCount: true, placeholder: '补充说明（可选）' }}
        />
      </ModalForm>
    </PageContainer>
  );
}
