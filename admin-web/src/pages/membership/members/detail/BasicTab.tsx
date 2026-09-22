import { ProDescriptions } from '@ant-design/pro-components';
import type { ProDescriptionsItemProps } from '@ant-design/pro-components';
import { Alert, Tag } from 'antd';
import { formatDateTime } from '../../../../services/datetime';
import { enumMeta } from '../../../../services/labels';
import type { Membership } from '../../../../services/membership';
import { MEMBERSHIP_STATUS, MEMBER_PRICE_MODE } from '../../../../services/membershipLabels';

/** 接口给的空值一律「—」：留白与空串在描述列表里分不出来。 */
const dash = (value?: string | null) => (value ? value : '—');

/**
 * 会员的基本信息。
 *
 * 这一页要回答的顺序是：**这个人还是不是会员 → 什么时候到期 → 到那一天还会不会自动扣款
 * → 如果被冻了/撤销了，是谁在什么时候为什么做的**。
 *
 * 有一件事必须在这一页说清楚，因为它和列表页是同一个坑：`status` 是「库里那一格写的」，
 * `active` 才是「此刻到底作不作数」。到期扫描还没跑到这一条时，status 还是 active 而
 * 到期时间已经过去了——顶上那条 Alert 就是为这一刻准备的，不然客服会照着状态答「是会员」。
 */
export default function BasicTab({ membership }: { membership: Membership }) {
  const columns: ProDescriptionsItemProps<Membership>[] = [
    {
      // 会员的身份就是这一个字段。会员库没有昵称也没有手机号，接口也不 join 用户服务
      // （跨库，且会员域不该依赖身份域），所以这里**不做**任何「补一个名字上来」的美化：
      // 那是会员域之外的事实，在这里拼出来只是一份没有归属的缓存。
      title: '用户 ID',
      dataIndex: 'userId',
      copyable: true,
    },
    {
      // 名字与编码都是**成交当时的快照**：套餐后来改了名，这里不跟着变——用户当初买的就是
      // 那个名字。两个都摆出来，是因为编码要拿去与订单、流水对账，而名字是给人读的。
      title: '套餐',
      dataIndex: 'planName',
      render: (_, row) => (
        <>
          {dash(row.planName)}
          <span style={{ color: '#8c8c8c', marginInlineStart: 8 }}>{row.planCode}</span>
        </>
      ),
    },
    {
      title: '状态',
      dataIndex: 'status',
      render: (_, row) => {
        const meta = enumMeta(MEMBERSHIP_STATUS, row.status);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      // 后端算好的那个「此刻作不作数」= status === 'active' 且还在有效期内。
      // **不要在前端用 status 与 expireAt 自己算**：那两列就在下面，看起来更"直观"，
      // 但把判断散到两边之后，一旦后端改了口径（比如将来把冻结也算进 active），
      // 这一页就是唯一一处还按老口径回答的地方。
      title: '此刻是否有效',
      dataIndex: 'active',
      render: (_, row) => (
        <span title="后端按「状态为生效中且在有效期内」算好的，与上面那一格不是一回事">
          {row.active ? <Tag color="success">有效</Tag> : <Tag>无效</Tag>}
        </span>
      ),
    },
    {
      // 成交快照，与套餐无关：它答的是「这个人是怎么享到会员价的」——自己直接享，
      // 还是靠每期发到账上的会员价体验券。客服接到「为什么我没享到会员价」要先看这一格。
      title: '会员价方式',
      dataIndex: 'memberPriceMode',
      render: (_, row) => {
        const meta = enumMeta(MEMBER_PRICE_MODE, row.memberPriceMode);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      title: '开通时间',
      dataIndex: 'startAt',
      render: (_, row) => formatDateTime(row.startAt),
    },
    {
      title: '到期时间',
      dataIndex: 'expireAt',
      render: (_, row) => formatDateTime(row.expireAt),
    },
    {
      /**
       * 归属门店：「这个人算哪家店的业绩」。**与会员权益无关**——会员价在哪家店用都一样，
       * 核销不看它，也不参与任何金额计算。
       *
       * 规则是「第一次成为会员那一刻固化，续费不覆盖；过期之后重新开通才可变」。所以这一格
       * 不跟着续费变，是设计而不是 bug。后台改不了它——只有成交能定（在门店买咖啡、店铺码活动）。
       *
       * 后端把门店名现解出来（会员库只存 id，名字是商户域的事实），解不出来是空串；
       * 那时候退回显示 id——至少那条引用还能对上账。两个都空才是「没有归属门店」。
       */
      title: '归属门店',
      dataIndex: 'storeName',
      render: (_, row) => dash(row.storeName || row.storeId),
    },
    {
      // 这个开关是**用户自己**能关的那个（关了就是到期不再扣款）。套餐支不支持自动续费
      // 是另一回事，在套餐页上——这里说「没开」不代表这个套餐不支持。
      title: '自动续费',
      dataIndex: 'autoRenew',
      render: (_, row) => (row.autoRenew ? <Tag color="blue">已开启</Tag> : <Tag>未开启</Tag>),
    },
    {
      title: '关闭自动续费时间',
      dataIndex: 'autoRenewOffAt',
      render: (_, row) => dash(row.autoRenewOffAt && formatDateTime(row.autoRenewOffAt)),
    },
    {
      // 首购不算续期，所以刚开通是 0。与流水对上才说明数据是干净的：流水里 renew 的条数
      // 应当等于这个数，对不上时先看流水里有没有重复的 renew。
      title: '续期次数',
      dataIndex: 'renewalCount',
      render: (_, row) => `${row.renewalCount} 次`,
    },
    {
      title: '最近续期',
      dataIndex: 'lastRenewedAt',
      render: (_, row) => dash(row.lastRenewedAt && formatDateTime(row.lastRenewedAt)),
    },
    // 冻结与撤销各是「时间 + 原因」两格，但**两格会不同时为空**，所以不能像出库单的作废
    // 那样合成一行：`frozenAt` 是真正的 null，而 `freezeReason` 是空串，解冻之后时间还在、
    // 原因也还在（它记的是上一次冻结）。分成两行，各自按各自的形式判空。
    {
      title: '冻结时间',
      dataIndex: 'frozenAt',
      render: (_, row) => dash(row.frozenAt && formatDateTime(row.frozenAt)),
    },
    {
      title: '冻结原因',
      dataIndex: 'freezeReason',
      span: 2,
      render: (_, row) => dash(row.freezeReason),
    },
    {
      title: '撤销时间',
      dataIndex: 'revokedAt',
      render: (_, row) => dash(row.revokedAt && formatDateTime(row.revokedAt)),
    },
    {
      title: '撤销原因',
      dataIndex: 'revokeReason',
      span: 2,
      render: (_, row) => dash(row.revokeReason),
    },
    {
      title: '创建时间',
      dataIndex: 'createdAt',
      render: (_, row) => formatDateTime(row.createdAt),
    },
    {
      title: '更新时间',
      dataIndex: 'updatedAt',
      render: (_, row) => formatDateTime(row.updatedAt),
    },
  ];

  return (
    <>
      {membership.status === 'revoked' ? (
        <Alert
          type="error"
          showIcon
          style={{ marginBottom: 15 }}
          message="这个会员已被撤销，动作不可逆"
          description={`撤销之后不能再做任何调整（后端对冻结、解冻、改有效期一律回 409），自动续费也一并关掉了。撤销原因：${
            membership.revokeReason || '—'
          }`}
        />
      ) : null}
      {membership.status === 'frozen' ? (
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 15 }}
          message="冻结中——会员价权益停用，到期时间没变"
          description={`冻结只是把权益按下了暂停，有效期照旧在走：解冻之后不会补时间，也不会因为冻结而少算。冻结原因：${
            membership.freezeReason || '—'
          }`}
        />
      ) : null}
      {membership.status === 'active' && !membership.active ? (
        // 这一条是「照状态答客诉会答错」的那一瞬间：到点扫描还没跑到这一行。
        <Alert
          type="info"
          showIcon
          style={{ marginBottom: 15 }}
          message="状态还是「生效中」，但已经过了到期时间"
          description="到期扫描把状态改成「已过期」之前会有这么一段窗口。以到期时间为准——上面那一格「此刻是否有效」显示的才是后端此刻的结论。"
        />
      ) : null}
      <ProDescriptions<Membership>
        column={2}
        dataSource={membership}
        columns={columns}
        // 这一屏是一次性把详情拿回来渲染的，没有再请求，所以不用 loading 态。
        bordered
        size="middle"
      />
    </>
  );
}
