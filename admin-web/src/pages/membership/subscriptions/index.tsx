import {
  ModalForm,
  PageContainer,
  ProFormTextArea,
  ProTable,
} from '@ant-design/pro-components';
import type { ActionType, ProColumns } from '@ant-design/pro-components';
import { useAccess } from '@umijs/max';
import { Alert, Button, Card, Col, Descriptions, Drawer, Row, Statistic, Table, Tag, message } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { useCallback, useEffect, useRef, useState } from 'react';
import { enumMeta, searchOptions } from '../../../services/labels';
import { formatYuan } from '../../../services/money';
import { periodLabel, SUBSCRIPTION_SCENE, SUBSCRIPTION_STATUS } from '../../../services/membershipLabels';
import { AGREEMENT_CHARGE_STATUS } from '../../../services/paymentLabels';
import { paymentMethodLabel } from '../../../services/paymentMethodLabels';
import { requestErrorMessage } from '../../../services/requestError';
import {
  cancelSubscription,
  getSubscription,
  getSubscriptionStats,
  listSubscriptions,
  syncSubscription,
  type Subscription,
  type SubscriptionCharge,
  type SubscriptionDetail,
  type SubscriptionQuery,
  type SubscriptionStats,
} from '../../../services/subscription';
import { scrollableModalBody } from '../../../components/common/modalProps';

/**
 * 包月订阅管理：谁签了连续包月、这一期扣了没。
 *
 * 这一页**只有读，加「同步」与「取消」**，与后端那棵树一致（见 services/subscription.ts 的
 * 文件头）。老后台还有「新建」，这里没有：订阅只可能由小程序签约产生，老后台同样建不了。
 *
 * **列表为空是有可能的**（还没有人在小程序里签过连续包月），但它不再等于「链路没接」——这一句
 * 得摆在页面上：一个空表格与一个坏掉的页面在运营眼里长得一模一样。
 */

/** 接口给的空值一律显示成「—」：留白与「有个空字符串」在表里分不出来。 */
const dash = (value?: string | null) => (value ? value : '—');

/**
 * 「同步」按钮该不该出现。还活着的三种状态才给：`pending_sign`（等用户去微信点同意）、
 * `active`、`suspended`。cancelled / expired 是终点，后端不让它们复活（回 409）。
 */
const isLive = (status: Subscription['status']) =>
  status === 'pending_sign' || status === 'active' || status === 'suspended';

/** 精确匹配的筛选参数：发出去之前 trim，空的整个丢掉。 */
const exact = (value: unknown): string | undefined => {
  const trimmed = typeof value === 'string' ? value.trim() : '';
  return trimmed || undefined;
};

export default function MembershipSubscriptionsPage() {
  const access = useAccess();
  const actionRef = useRef<ActionType>();

  /** 头上那两张卡。单独一个请求（见后端的说明），所以它的刷新与表格是两回事。 */
  const [stats, setStats] = useState<SubscriptionStats>();

  /** 详情抽屉里那一条。抽屉打开时单独取一次详情——列表那一行只带得动列表要用的字段。 */
  const [detail, setDetail] = useState<SubscriptionDetail>();
  const [detailOpen, setDetailOpen] = useState(false);
  /** 详情请求的失败信息。**只有详情本身取不到才算失败**，抽屉里那两块回显各有各的降级。 */
  const [detailError, setDetailError] = useState(false);

  /** 正在取消的那一条（弹窗打开时才有值）。 */
  const [cancelTarget, setCancelTarget] = useState<Subscription>();

  /** 正在同步的那一条（按钮转圈用，防连点）。 */
  const [syncingId, setSyncingId] = useState<string>();

  const loadStats = useCallback(async () => {
    try {
      setStats(await getSubscriptionStats());
    } catch {
      // 统计挂了不该把页面拦下来：表格本身是可用的。两个数显示成空白，运营点一下刷新就会
      // 再试一次——比整页报错好。
      setStats(undefined);
    }
  }, []);

  useEffect(() => {
    loadStats();
  }, [loadStats]);

  /**
   * 取一条详情并塞进抽屉。
   *
   * 失败不弹 toast，只把抽屉里那一块换成一句话：**详情本身取不到**（本库那条读不出来、
   * 或者这条订阅不存在）才是失败，而那件事在抽屉里说比在页面角落闪一下更清楚。抽屉里另外
   * 两块（续费明细、首月支付信息）各有各的降级——支付域读不到时后端回的是空数组而不是错误。
   */
  const loadDetail = useCallback(async (id: string) => {
    try {
      setDetail(await getSubscription(id));
    } catch {
      setDetailError(true);
    }
  }, []);

  const openDetail = (id: string) => {
    setDetailOpen(true);
    // 先清空再取：抽屉打开时还留着上一条的内容，看起来像是点错了行。
    setDetail(undefined);
    setDetailError(false);
    loadDetail(id);
  };

  /**
   * 同步一条订阅：拿协议号回渠道核一次，按渠道的结论纠正本地。
   *
   * **两个结果都是成功**，所以提示语必须分开说：`changed` 为真才是「已同步」，为假要讲清楚
   * 为什么没变（渠道还在等用户点同意 / 本地早就是那个状态了）——一句笼统的「同步完成」会让
   * 运营以为那条 pending 的单子已经生效了。
   */
  const sync = async (row: Subscription) => {
    setSyncingId(row.id);
    try {
      const result = await syncSubscription(row.id);
      if (result.changed) {
        message.success('已按渠道的状态更新这条订阅');
      } else if (result.providerState === 'pending') {
        message.info('渠道那边还在等用户确认，本地状态不变');
      } else {
        message.info('本地已经是渠道说的状态，无需更正');
      }
      // 抽屉正开着这一条时顺手刷新它：同步改的字段（状态、下一期扣款时间）正是抽屉里显示的
      // 那几个，不刷新的话抽屉里还是旧值，与刚弹出来的提示自相矛盾。
      //
      // **重取一次而不是拿 result.subscription 塞进去**：那个字段是列表那一条的形状，没有
      // 协议号与续费明细那三块，塞进去会让抽屉下半截突然空掉。
      if (detail?.id === row.id) {
        loadDetail(row.id);
      }
      actionRef.current?.reload();
      loadStats();
    } catch (error) {
      // 话照搬后端那句（渠道说不通时是「支付渠道暂时不可用，请稍后再试」这类中文），吞掉它换成
      // 「同步失败」等于让运营去猜是本地还是渠道的问题。
      message.error(requestErrorMessage(error, '同步失败，请稍后重试'));
    } finally {
      setSyncingId(undefined);
    }
  };

  /**
   * 续费明细那张表：**某一期扣到哪一步了**。
   *
   * 与列表那几张表不同，它不在 ProTable 上、也没有筛选——它是抽屉里的一段只读回显，数据
   * 随详情一起回来（详情一次取齐）。所以列定义只是个常量，不参与 request。
   *
   * 六列里有三列是「出了事才看得懂」的：期次（对账时与渠道那张表按它对齐）、渠道流水（拿去
   * 微信商户平台查这一笔）、失败原因。失败原因放最后并允许折行——它是唯一一个长度不可控的
   * 字段，卡在中间会把右边几列挤出去。
   */
  const chargeColumns: ColumnsType<SubscriptionCharge> = [
    {
      title: '期次',
      dataIndex: 'bizPeriod',
      width: 100,
    },
    {
      // **扣款时间取 chargedAt，没有就留空**：失败与进行中的期次没有「扣款时间」这个事实，
      // 拿 createdAt（这一行什么时候建的）顶上去会让人以为那一期真的在那个时刻扣过。
      title: '扣款时间',
      dataIndex: 'chargedAt',
      width: 180,
      render: (_, row) => dash(row.chargedAt),
    },
    {
      title: '金额',
      dataIndex: 'amount',
      width: 100,
      render: (_, row) => `¥${formatYuan(row.amount)}`,
    },
    {
      title: '状态',
      dataIndex: 'status',
      width: 140,
      render: (_, row) => {
        const meta = enumMeta(AGREEMENT_CHARGE_STATUS, row.status);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      // 渠道流水只在成功与进行中的期次上有；未来得及受理的期次是空的。
      title: '渠道流水',
      dataIndex: 'providerTransactionId',
      ellipsis: true,
      render: (_, row) => dash(row.providerTransactionId),
    },
    {
      // 失败原因：给运营看的那句话，机器码（failureCode）不展示——它是给日志和排查用的，
      // 摆在页面上只会多一个要解释的英文缩写。
      title: '失败原因',
      dataIndex: 'failureMessage',
      ellipsis: true,
      render: (_, row) => dash(row.failureMessage),
    },
  ];

  const columns: ProColumns<Subscription>[] = [
    {
      title: '用户 ID',
      dataIndex: 'userId',
      copyable: true,
      ellipsis: true,
      width: 220,
      // 等值比较（uuid 列），给不出候选，所以是输入框。
      fieldProps: { placeholder: '完整用户 ID' },
    },
    {
      title: '订阅状态',
      dataIndex: 'status',
      valueType: 'select',
      valueEnum: searchOptions(SUBSCRIPTION_STATUS),
      width: 100,
      render: (_, row) => {
        const meta = enumMeta(SUBSCRIPTION_STATUS, row.status);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      // V2 没有 VIP 等级这一层，等价物就是套餐——所以这一列显示套餐名而不是等级名。
      title: '会员等级',
      dataIndex: 'planName',
      ellipsis: true,
      search: false,
      width: 150,
    },
    {
      title: '周期',
      dataIndex: 'period',
      search: false,
      width: 110,
      render: (_, row) => periodLabel(row.period, row.periodCount),
    },
    {
      // 金额是**签约时的快照**：套餐后来调价，已签约的人仍按这个数扣。
      title: '每期金额',
      dataIndex: 'priceCents',
      search: false,
      width: 110,
      render: (_, row) => `¥${formatYuan(row.priceCents)}`,
    },
    {
      title: '签约场景',
      dataIndex: 'signScene',
      search: false,
      width: 140,
      render: (_, row) => {
        const meta = enumMeta(SUBSCRIPTION_SCENE, row.signScene);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      // 老后台这一列的原文。**只在会员中心签约那一档有值**：另外两条路上首月那笔钱在咖啡
      // 订单或活动单里，不在订阅上，硬摆过来会让人以为那是订阅的首期扣款。
      title: '首月支付',
      dataIndex: 'firstPaymentOrderId',
      search: false,
      ellipsis: true,
      width: 160,
      render: (_, row) => dash(row.firstPaymentOrderId),
    },
    {
      title: '续费次数',
      dataIndex: 'chargeCount',
      search: false,
      width: 90,
    },
    {
      // 三个按钮，所以宽度取 220（与本仓另外两个三按钮的页一致：user-coupons、admin-users），
      // 下面 scroll.x 跟着加（钉右列的不变式：fixed 列必须显式 width，scroll.x = 各列 width 之和）。
      title: '操作',
      valueType: 'option',
      width: 220,
      fixed: 'right',
      render: (_, row) => [
        <Button key="detail" type="link" size="small" onClick={() => openDetail(row.id)}>
          详情
        </Button>,
        // 同步只出现在「还活着」的三种状态上。**cancelled / expired 不给**：那两种已经结束了，
        // 后端会拒绝把它改回 active（回 409），按钮摆在那里只会让人以为系统坏了。
        isLive(row.status) && access.canManageMembership ? (
          <Button key="sync" type="link" size="small" loading={syncingId === row.id} onClick={() => sync(row)}>
            同步
          </Button>
        ) : null,
        // 只有 active 与 suspended 能取消（后端也这么判，回 409）。其余状态下这个按钮不出现
        // ——一个点了必然失败的按钮只会让人以为系统坏了。
        //
        // **suspended 特别需要它**：那一档是「连续扣款失败到停扣」，除了等下一次扣款成功自己
        // 回来没有别的出路，而想彻底不续的用户与想收尾的运营都只能按这个按钮。
        (row.status === 'active' || row.status === 'suspended') && access.canManageMembership ? (
          <Button key="cancel" type="link" size="small" danger onClick={() => setCancelTarget(row)}>
            取消
          </Button>
        ) : null,
      ],
    },
  ];

  return (
    <PageContainer
      title="包月订阅管理"
      content="用户在小程序里签约连续包月之后，这里能看到他签的是哪一档、下一期什么时候扣、扣成功了几次。后台不建订阅，也只能取消——取消会先把微信那边的代扣协议解掉，成功了才改本地；渠道没同意就什么都不变，会提示你重试。"
    >
      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 16 }}
        message="列表为空是正常的"
        description="订阅只能由用户在小程序里签约产生（这里看不到任何数据不代表页面坏了）。刚发起的签约停在「待签约」，默认列表不显示这一档——想看它们要在上面的状态里选「待签约」，或者在用户 ID 那一栏按人查。等用户去微信点完同意，或者在这里点一下「同步」问渠道要结果。"
      />

      <Row gutter={16} style={{ marginBottom: 16 }}>
        <Col span={8}>
          <Card>
            <Statistic title="有效订阅" value={stats?.activeCount ?? '—'} />
          </Card>
        </Col>
        <Col span={8}>
          <Card>
            {/* 「待续费」是已经到点该扣、但还没扣的条数（后端每 10 分钟扫一轮去发起扣款）。
                正常时它应该很快归零，**持续不归零才是要看的那件事**：要么扣款一直在失败，
                要么扫描没在跑。 */}
            <Statistic title="待续费" value={stats?.dueCount ?? '—'} />
          </Card>
        </Col>
      </Row>

      <ProTable<Subscription>
        actionRef={actionRef}
        rowKey="id"
        columns={columns}
        // 1300 = 220+100+150+110+110+140+160+90+220，各列 width 之和。改任何一列的宽度都要
        // 同批改这个数。
        scroll={{ x: 1300 }}
        search={{ labelWidth: 'auto' }}
        request={async (params) => {
          const query: SubscriptionQuery = {
            page: params.current,
            pageSize: params.pageSize,
            userId: exact(params.userId),
            status: exact(params.status) as SubscriptionQuery['status'],
          };
          const result = await listSubscriptions(query);
          return { data: result.items, total: result.total, success: true };
        }}
      />

      {/*
        宽度按抽屉里内容最多的那一块取：续费明细是一张五列的表（期次 / 扣款时间 / 金额 /
        状态 / 渠道流水 / 失败原因），520 摆不下，会挤成每个字一行。
      */}
      <Drawer
        width={800}
        open={detailOpen}
        onClose={() => setDetailOpen(false)}
        title="订阅详情"
        destroyOnClose
      >
        {detail ? (
          <>
            <Descriptions column={1} size="small" bordered>
              <Descriptions.Item label="用户 ID">{detail.userId}</Descriptions.Item>
              <Descriptions.Item label="订阅状态">
                {enumMeta(SUBSCRIPTION_STATUS, detail.status).text}
              </Descriptions.Item>
              <Descriptions.Item label="会员等级">{dash(detail.planName)}</Descriptions.Item>
              <Descriptions.Item label="周期">
                {periodLabel(detail.period, detail.periodCount)}
              </Descriptions.Item>
              <Descriptions.Item label="每期金额">¥{formatYuan(detail.priceCents)}</Descriptions.Item>
              <Descriptions.Item label="签约场景">
                {enumMeta(SUBSCRIPTION_SCENE, detail.signScene).text}
              </Descriptions.Item>
              {/*
                协议号在渠道那边就是这份约的标识，出了争议时运营拿它去微信商户平台查。
                活动发放与后台开通那两条路给的是一段会员、没有渠道协议，那种情况下是空串
                ——「—」在那里是对的，不是加载失败。
              */}
              <Descriptions.Item label="协议号">{dash(detail.contractCode)}</Descriptions.Item>
              <Descriptions.Item label="首月支付">
                {dash(detail.firstPaymentOrderId)}
              </Descriptions.Item>
              <Descriptions.Item label="下次扣款">{dash(detail.nextChargeAt)}</Descriptions.Item>
              <Descriptions.Item label="上次扣款">{dash(detail.lastChargeAt)}</Descriptions.Item>
              <Descriptions.Item label="扣款次数">
                {detail.chargeCount} 次（累计失败 {detail.failedCount} 次，连续失败{' '}
                {detail.consecutiveFailedCount} 次）
              </Descriptions.Item>
              <Descriptions.Item label="签约时间">{detail.createdAt}</Descriptions.Item>
              {detail.cancelAt ? (
                <>
                  <Descriptions.Item label="取消时间">{detail.cancelAt}</Descriptions.Item>
                  <Descriptions.Item label="取消原因">{dash(detail.cancelReason)}</Descriptions.Item>
                  <Descriptions.Item label="取消人">{dash(detail.cancelledBy)}</Descriptions.Item>
                </>
              ) : null}
            </Descriptions>

            <Card title="续费明细" size="small" style={{ marginTop: 16 }}>
              {/*
                空态是「这一份协议下还没有期次」**或**「支付域没读到」——后端两种情况都回空
                数组，页面上分不出来（要分得清就得让读失败也报错，而那会把整页打成 500）。
                所以文案不写「暂无」，写「没有可显示的期次」，别让运营以为这是承诺。
              */}
              <Table<SubscriptionCharge>
                rowKey="bizPeriod"
                size="small"
                columns={chargeColumns}
                dataSource={detail.charges}
                pagination={false}
                locale={{ emptyText: '没有可显示的续费期次' }}
              />
            </Card>

            {/*
              首月支付信息**整块跟着数据走**：没有首月订单时后端回 null，这里就不渲染。
              今天它恒为 null（要小程序端把首月订单号写进订阅行），所以这一块暂时看不到
              ——不是坏了，是那条链路还没接。
            */}
            {detail.firstPayment ? (
              <Card title="首月支付信息" size="small" style={{ marginTop: 16 }}>
                <Descriptions column={1} size="small" bordered>
                  <Descriptions.Item label="首月订单号">
                    {dash(detail.firstPayment.orderNo)}
                  </Descriptions.Item>
                  <Descriptions.Item label="订单状态">
                    {dash(detail.firstPayment.status)}
                  </Descriptions.Item>
                  <Descriptions.Item label="实付金额">
                    ¥{formatYuan(detail.firstPayment.paidAmount)}
                  </Descriptions.Item>
                  <Descriptions.Item label="支付方式">
                    {paymentMethodLabel(detail.firstPayment.paymentMethod)}
                  </Descriptions.Item>
                  <Descriptions.Item label="支付单号">
                    {dash(detail.firstPayment.paymentNo)}
                  </Descriptions.Item>
                  {/*
                    这一格是**渠道流水号**（provider_transaction_id），不是「微信流水」。
                    老后台那一列写的是微信流水，因为当年只有微信代扣一条路；这里的首月
                    可能是支付宝、也可能是咖啡豆（豆付的单没有渠道流水，恒为空）。同一个
                    抽屉里「续费明细」那列的标题已经是「渠道流水」，两处对齐。
                  */}
                  <Descriptions.Item label="渠道流水">
                    {dash(detail.firstPayment.providerTransactionId)}
                  </Descriptions.Item>
                  <Descriptions.Item label="支付时间">
                    {dash(detail.firstPayment.paidAt)}
                  </Descriptions.Item>
                </Descriptions>
              </Card>
            ) : null}
          </>
        ) : detailError ? (
          // 详情本身取不到才是失败（本库那条读不出来，或者这条订阅已经不在了）。抽屉里说
          // 一句就够——页面本身没坏，列表还在。
          <Alert
            type="error"
            showIcon
            message="取不到这条订阅的详情"
            description="列表本身是可用的，可以关掉抽屉重开一次试试。若一直是这一条，多半是后端或这条订阅本身的问题。"
          />
        ) : (
          <Card loading />
        )}
      </Drawer>

      {/*
        取消要走一个弹窗填原因：这是别人钱袋子上的一次人工操作，事后留痕里不该有一句系统替他
        写的话（后端也会拒空原因）。**key 与 destroyOnClose 两个都要有**——缺了它们，跨行
        操作时上一个弹窗里的原因会被带到下一条订阅上。
      */}
      <ModalForm
        key={`cancel-${cancelTarget?.id ?? ''}`}
        title="取消包月订阅"
        open={!!cancelTarget}
        width={420}
        modalProps={{
          ...scrollableModalBody,
          destroyOnClose: true,
          maskClosable: false,
          onCancel: () => setCancelTarget(undefined),
        }}
        onFinish={async (values: { reason?: string }) => {
          if (!cancelTarget) return true;
          try {
            await cancelSubscription(cancelTarget.id, values.reason ?? '');
            message.success('订阅已取消');
            setCancelTarget(undefined);
            // 详情抽屉里那条也可能是它，重新取一次；表格与统计一起刷。
            if (detail?.id === cancelTarget.id) {
              setDetail(await getSubscription(cancelTarget.id));
            }
            actionRef.current?.reload();
            loadStats();
            return true;
          } catch (error) {
            // 失败时**不关弹窗**：原因还留着，运营可以直接改一改再提交（多半是状态已经变了）。
            //
            // 话照搬后端那句：状态已经变了的时候，后端会回「订阅状态为 X，无需取消」，
            // 那正是运营需要看到的一句；吞掉它换成「取消失败」等于让人去猜。
            message.error(requestErrorMessage(error, '取消失败，请刷新后重试'));
            return false;
          }
        }}
      >
        <ProFormTextArea
          name="reason"
          label="取消原因"
          rules={[{ required: true, message: '请填写取消原因' }]}
          fieldProps={{ rows: 3, maxLength: 200, showCount: true }}
          extra="会记进这条订阅的取消留痕里，也会作为解约原因报给微信。取消不退款、不改会员有效期，只是不让他下个月再被扣；微信没同意解约时会整条失败，什么都不变。"
        />
      </ModalForm>
    </PageContainer>
  );
}
