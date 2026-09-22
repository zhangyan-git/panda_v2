import { PageContainer, ProTable } from '@ant-design/pro-components';
import type { ActionType, ProColumns } from '@ant-design/pro-components';
import { Button, Drawer, Descriptions, Empty, message, Table, Tag, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { useEffect, useRef, useState } from 'react';
import { formatDateTime } from '../../../services/datetime';
import { enumMeta, searchOptions } from '../../../services/labels';
import {
  getWin,
  listCampaigns,
  listWins,
  type Campaign,
  type Win,
  type WinDetail,
  type WinEvent,
  type WinQuery,
} from '../../../services/lottery';
import {
  LOTTERY_ACTOR_TYPE,
  WIN_EVENT_TYPE,
  WIN_STATUS,
} from '../../../services/lotteryLabels';
import { FULL_PAGE_PARAMS } from '../../../services/pagination';
import { requestErrorMessage } from '../../../services/requestError';

/**
 * 中奖记录（只读）。
 *
 * **这一页本轮没有写动作**，一个都没有：核销、领取、换奖整块延后到下一轮，中奖一律停在
 * 「待领取」。所以界面上不摆任何看起来能操作的按钮——一个点了没反应的「核销」比没有更糟。
 * 下一轮的写入口也**不在后台**，在商户端（`/v1/merchant/lottery/...`）：后台替用户核销要
 * 单独设计「谁在什么情况下能替」，那是另一件事。
 *
 * 凭证号（claimNo）在这里看得见但**不是凭据**：它不校验、不作鉴权，用户将来在门店报的
 * 就是它。与订单的取杯号同一个性质。
 */

const dash = (value?: string | null) => (value ? value : '—');
const time = (value?: string | null) => (value ? formatDateTime(value) : '—');

const exact = (value: unknown): string | undefined => {
  const trimmed = typeof value === 'string' ? value.trim() : '';
  return trimmed || undefined;
};

export default function LotteryWinsPage() {
  const actionRef = useRef<ActionType>();
  const [campaigns, setCampaigns] = useState<Campaign[]>([]);
  const [detail, setDetail] = useState<WinDetail>();
  const [loadingDetail, setLoadingDetail] = useState(false);

  useEffect(() => {
    void (async () => {
      try {
        const { items } = await listCampaigns(FULL_PAGE_PARAMS);
        setCampaigns(items);
      } catch {
        // 忽略：拿不到就只是筛选下拉是空的。
      }
    })();
  }, []);

  /**
   * 看一条中奖的来龙去脉。
   *
   * 走的是详情接口而不是列表里那一行：**流水不在列表里**，而「这条奖是谁、什么时候、
   * 因为什么产生的」正是 lottery_win_events 那张只增表存在的理由。本轮那张表里只会有一条
   * created（actor 是 system，因为开奖是收满门槛触发的，没有人「做了」这件事）。
   */
  const openDetail = async (id: string) => {
    setLoadingDetail(true);
    try {
      setDetail(await getWin(id));
    } catch (error) {
      message.error(requestErrorMessage(error, '加载中奖详情失败'));
    } finally {
      setLoadingDetail(false);
    }
  };

  const eventColumns: ColumnsType<WinEvent> = [
    {
      title: '时间',
      dataIndex: 'createdAt',
      width: 170,
      render: (_, row) => time(row.createdAt),
    },
    {
      title: '事件',
      dataIndex: 'eventType',
      width: 140,
      render: (_, row) => {
        const meta = enumMeta(WIN_EVENT_TYPE, row.eventType);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      title: '状态变化',
      dataIndex: 'toStatus',
      width: 160,
      // 空串是「从无到有」（created 那一条的 from_status 就是空），显示成「—」而不是
      // 留白：留白看着像这一格没数据，而这里恰恰是在说「之前没有状态」。
      render: (_, row) => `${dash(row.fromStatus)} → ${dash(row.toStatus)}`,
    },
    {
      title: '操作者',
      dataIndex: 'actorType',
      width: 110,
      render: (_, row) => {
        const meta = enumMeta(LOTTERY_ACTOR_TYPE, row.actorType);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    { title: '原因', dataIndex: 'reason', ellipsis: true, render: (_, row) => dash(row.reason) },
  ];

  const columns: ProColumns<Win>[] = [
    {
      // 用户将来在门店要报的就是这个号。放在第一列是因为客服最常被问的就是它。
      title: '凭证号',
      dataIndex: 'claimNo',
      copyable: true,
      width: 170,
      fieldProps: { placeholder: '完整凭证号' },
    },
    {
      title: '期次号',
      dataIndex: 'roundId',
      ellipsis: true,
      width: 150,
      fieldProps: { placeholder: '完整期次 ID' },
      render: (_, row) => dash(row.roundNo),
    },
    {
      title: '活动',
      dataIndex: 'campaignId',
      valueType: 'select',
      valueEnum: Object.fromEntries(
        campaigns.map((item) => [item.id, { text: `${item.name}（${item.locationName}）` }]),
      ),
      ellipsis: true,
      width: 160,
      render: (_, row) => dash(row.campaignName),
    },
    {
      title: '用户 ID',
      dataIndex: 'userId',
      copyable: true,
      ellipsis: true,
      width: 280,
      fieldProps: { placeholder: '完整用户 ID' },
    },
    {
      // 这里原先还有一个奖品类型的 Tag（PRIZE_KIND），2026-09-15 随类型那一列删了。
      title: '奖品',
      dataIndex: 'prizeId',
      search: false,
      ellipsis: true,
      width: 200,
      // 显示 current 而不是 original：换奖之后用户手上的是现奖品。原奖品在详情里
      // 分两行摆出来，那一列才是「当时开出来的是什么」。本轮两者相同（没有换奖）。
      render: (_, row) => row.currentPrizeName,
    },
    {
      // 本轮只可能出现 pending——核销整块延后了（其余取值是最终状态机的一部分，先登记着，
      // 见 lotteryLabels 里那段说明）。
      title: '状态',
      dataIndex: 'status',
      valueType: 'select',
      valueEnum: searchOptions(WIN_STATUS),
      width: 100,
      render: (_, row) => {
        const meta = enumMeta(WIN_STATUS, row.status);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      title: '来源订单',
      dataIndex: 'sourceOrderNo',
      search: false,
      ellipsis: true,
      width: 190,
      render: (_, row) => dash(row.sourceOrderNo),
    },
    {
      title: '中奖时间',
      dataIndex: 'createdAt',
      valueType: 'dateTime',
      search: false,
      width: 170,
    },
    {
      title: '操作',
      valueType: 'option',
      // fixed 的列必须显式给宽度，且下面的 scroll.x 必须等于各列宽度之和（见下面的 1420）。
      // 110 是量出来的：「查看详情」一个 link 按钮内容宽约 88，加左右各 8px 内边距。
      width: 110,
      fixed: 'right',
      render: (_, row) => [
        <Button
          key="detail"
          type="link"
          size="small"
          loading={loadingDetail}
          onClick={() => void openDetail(row.id)}
        >
          查看详情
        </Button>,
      ],
    },
  ];

  return (
    <PageContainer
      title="中奖记录"
      content="开奖时按记录在案的种子与参与集合算出，一经开出不可更改。核销与领取尚未开放，本轮所有中奖都停在「待领取」。"
    >
      <ProTable<Win>
        actionRef={actionRef}
        rowKey="id"
        columns={columns}
        // 1420 = 170+150+160+280+200+100+190+170+110，各列 width 之和。
        scroll={{ x: 1420 }}
        search={{ labelWidth: 'auto' }}
        options={false}
        request={async (params) => {
          const query: WinQuery = {
            page: params.current,
            pageSize: params.pageSize,
            roundId: exact(params.roundId),
            campaignId: exact(params.campaignId),
            userId: exact(params.userId),
            status: exact(params.status) as WinQuery['status'],
            claimNo: exact(params.claimNo),
          };
          const result = await listWins(query);
          return { data: result.items, total: result.total, success: true };
        }}
      />

      <Drawer
        title={detail ? `中奖详情 ${detail.win.claimNo}` : '中奖详情'}
        open={!!detail}
        onClose={() => setDetail(undefined)}
        width={720}
      >
        {detail ? (
          <>
            <Descriptions column={2} size="small" style={{ marginBottom: 16 }}>
              <Descriptions.Item label="用户 ID" span={2}>
                <Typography.Text copyable>{detail.win.userId}</Typography.Text>
              </Descriptions.Item>
              <Descriptions.Item label="状态">
                {enumMeta(WIN_STATUS, detail.win.status).text}
              </Descriptions.Item>
              <Descriptions.Item label="中奖时间">{time(detail.win.createdAt)}</Descriptions.Item>
              {/* 原奖品与现奖品分两行：换奖改的是现奖品，原奖品永远留着——它是「当时开
                  出来的是哪个奖」的唯一记录。本轮两者相同。 */}
              <Descriptions.Item label="原奖品">
                {dash(detail.win.originalPrizeName)}
              </Descriptions.Item>
              <Descriptions.Item label="现奖品">
                {dash(detail.win.currentPrizeName)}
              </Descriptions.Item>
              <Descriptions.Item label="期次号">{dash(detail.win.roundNo)}</Descriptions.Item>
              <Descriptions.Item label="活动">{dash(detail.win.campaignName)}</Descriptions.Item>
              <Descriptions.Item label="来源订单" span={2}>
                {dash(detail.win.sourceOrderNo)}
              </Descriptions.Item>
              <Descriptions.Item label="领取截止">
                {/* null = 不过期，与福卡一致。本轮恒为 null，所以这一行现在只说明「不会过期」。 */}
                {detail.win.expiresAt ? time(detail.win.expiresAt) : '不过期'}
              </Descriptions.Item>
              <Descriptions.Item label="核销信息">
                {/* 核销整块延后，所以这一格现在恒为「—」。摆出来是为了让下一轮加核销时
                    不必回来加一行，也让人看得出「这里将来会有东西」。 */}
                {dash(detail.win.redeemLocationName || detail.win.redeemedAt)}
              </Descriptions.Item>
            </Descriptions>
            <Typography.Title level={5}>流水</Typography.Title>
            <Table<WinEvent>
              rowKey="id"
              size="small"
              pagination={false}
              columns={eventColumns}
              dataSource={detail.events ?? []}
              locale={{ emptyText: <Empty description="没有流水" /> }}
            />
          </>
        ) : null}
      </Drawer>
    </PageContainer>
  );
}
