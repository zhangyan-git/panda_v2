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
  ProFormDigit,
  ProFormTextArea,
  ProTable,
} from '@ant-design/pro-components';
import type { ActionType, ProColumns } from '@ant-design/pro-components';
import { useAccess } from '@umijs/max';
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
 * 一笔流水的金额文案。必须带符号：这个数是有符号的（调整充值为正、扣减为负），
 * 只显示绝对值会让一次扣减看起来像又充了一笔。
 */
const beanAmountText = (amount: number) => (amount > 0 ? `+${amount}` : `${amount}`);

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

  // 余额调整
  const [beanAdjustOpen, setBeanAdjustOpen] = useState(false);
  // 幂等键，跟着「打开弹窗」走：同一个 requestId 重发只会记一次账，所以它必须在用户改金额
  // 时保持不动，只在这一轮调整结束时才换。
  const [beanRequestId, setBeanRequestId] = useState('');

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

  const openDetail = async (id: string) => {
    try {
      setDetail(await getMiniappUser(id));
    } catch (error) {
      message.error(requestErrorMessage(error, '加载详情失败'));
      return;
    }
    // 余额单独拉、单独失败：它是另一个上游（account-service）的另一件事，读不到只是那一个
    // 区块显示「读不到余额」，不该让整个抽屉打不开——没有 account:read 的账号照样要能看用户。
    setBeanAccount(undefined);
    await refreshBeanAccount(id);
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
      title: '金额（分）',
      dataIndex: 'amount',
      width: 100,
      render: (_, row) => (
        <span style={{ color: row.amount > 0 ? '#3f8600' : '#cf1322' }}>
          {beanAmountText(row.amount)}
        </span>
      ),
    },
    {
      // 「变动后」而不是当前余额：冲正重放时只有它能回答「当时是多少」。
      title: '变动后（分）',
      dataIndex: 'balanceAfter',
      width: 110,
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

            {/* 咖啡豆账户（account-service 的用户维度余额，单位分）。
                与上面两块不同，它不是用户服务给的：`detail` 里没有余额，要另发一次请求。
                所以这里既可能「还没拉到」，也可能「拉不到」（没有 account:read 时），
                两种状态分开写，别把读失败显示成 0 分——那会让人以为用户真的一分没有。 */}
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
                    余额：<strong>{beanAccount.balance}</strong> 分
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

      {/* 余额调整。单位是**分**、金额**带符号**（充值为正、纠错为负），没有「方向」那一栏：
          豆的金额本身就是分，而读它的地方（订单金额、流水）全按分看，这里换成元再让方向决定
          符号，只会多出一处单位错位的机会。 */}
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
          const amount = Number(values.amount);
          if (!Number.isInteger(amount) || amount === 0) {
            message.error('调整金额必须是不为 0 的整数（单位：分）');
            return false;
          }
          try {
            const result = await adjustCoffeeBeans(detail.id, {
              amount,
              requestId: beanRequestId,
              remark: values.remark,
            });
            message.success(`已调整，当前余额 ${result.balance} 分`);
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
                  ? `这次调整的流水已经记过账（上一次多半已经成功），当前余额 ${latest.balance} 分，请核对`
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
            const after = beanAccount.balance + Number(amount ?? 0);
            return (
              <div style={{ marginBottom: 16 }}>
                <Space size="large">
                  <span>
                    当前余额：<strong>{beanAccount.balance}</strong> 分
                  </span>
                  <span>
                    调整后：
                    <strong style={{ color: after < 0 ? '#cf1322' : undefined }}>
                      {after}
                    </strong>{' '}
                    分
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
          min={-100000000}
          max={100000000}
          fieldProps={{ precision: 0, step: 1, style: { width: '100%' } }}
          extra="单位：分，可填负数（充值为正，把充错的豆调回来为负）"
          rules={[{ required: true, message: '请输入调整金额' }]}
        />
        <ProFormTextArea
          name="remark"
          label="备注"
          tooltip="余额调整全程留痕，备注会跟着这次操作一起记进流水"
          placeholder="例如：活动补偿 / 充错了调回"
        />
      </ModalForm>
    </PageContainer>
  );
}
