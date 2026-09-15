import { PageContainer } from '@ant-design/pro-components';
import { history, useParams } from '@umijs/max';
import { Card, Descriptions, Empty, Table, Tag, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { useCallback, useEffect, useState } from 'react';
import { formatDateTime } from '../../../../services/datetime';
import { enumMeta } from '../../../../services/labels';
import {
  getCampaign,
  listRounds,
  type Campaign,
  type CampaignPrize,
  type Round,
} from '../../../../services/lottery';
import {
  CAMPAIGN_STATUS,
  PRIZE_KIND,
  ROUND_STATUS,
  roundProgressLabel,
  winnerCountLabel,
} from '../../../../services/lotteryLabels';
import { FULL_PAGE_PARAMS } from '../../../../services/pagination';
import { requestErrorMessage } from '../../../../services/requestError';

/**
 * 活动详情：活动本身 + 它的奖池 + 它开过的期次。
 *
 * **奖池只有这一条路能看到**：列表接口有意不带 prizes（一次 20 个活动、每个带 5 个奖品，
 * 列表就成了奖池查询），所以想核对「名额配得对不对」必须进这一页。
 *
 * 期次是滚出来的，没有「建一期」的接口，所以这一页在这一块**只读**：能动的两个动作
 * （开奖 / 作废）在期次列表页上，因为它们作用于某**一期**而不是整个活动。
 */

const dash = (value?: string | null) => (value ? value : '—');
const time = (value?: string | null) => (value ? formatDateTime(value) : '—');

export default function CampaignDetailPage() {
  const { id } = useParams<{ id: string }>();
  const [campaign, setCampaign] = useState<Campaign>();
  const [rounds, setRounds] = useState<Round[]>([]);
  const [loading, setLoading] = useState(true);
  // 取不到时的原因（id 是手敲的、或不是 uuid 时后端会回 500，这个是已知缺口）。
  const [error, setError] = useState<string>();

  const load = useCallback(async () => {
    if (!id) return;
    setLoading(true);
    try {
      const [detail, roundPage] = await Promise.all([
        getCampaign(id),
        // 期次要单独取一次：活动详情里不带它（一份活动带 N 期，N 是涨的）。
        listRounds({ campaignId: id, ...FULL_PAGE_PARAMS }),
      ]);
      setCampaign(detail);
      setRounds(roundPage.items);
      setError(undefined);
    } catch (err) {
      setError(requestErrorMessage(err, '加载活动失败'));
    } finally {
      setLoading(false);
    }
  }, [id]);

  useEffect(() => {
    void load();
  }, [load]);

  const prizeColumns: ColumnsType<CampaignPrize> = [
    {
      // 行序就是发奖顺序：第一档拿满自己的名额才轮到下一档（算法的输入顺序，
      // 而那个顺序由这里的行序决定）。所以这一列要显示出来，它不是装饰。
      title: '顺序',
      dataIndex: 'sortOrder',
      width: 70,
      render: (_, row, index) => index + 1,
    },
    {
      title: '类型',
      dataIndex: 'prizeKind',
      width: 100,
      render: (_, row) => {
        const meta = enumMeta(PRIZE_KIND, row.prizeKind);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    { title: '奖品名称', dataIndex: 'name', width: 220, ellipsis: true },
    {
      title: '名额',
      dataIndex: 'quantity',
      width: 90,
      render: (_, row) => `${row.quantity} 个`,
    },
    {
      title: '领取说明',
      dataIndex: 'claimInstructions',
      ellipsis: true,
      render: (_, row) => dash(row.claimInstructions),
    },
  ];

  // 这几张表是 antd 的 Table（不是 ProTable），所以列类型是 ColumnsType——它**不认**
  // ProColumns 的 copyable。要能复制就自己包一层 Typography.Text。
  const roundColumns: ColumnsType<Round> = [
    {
      title: '期次号',
      dataIndex: 'roundNo',
      width: 150,
      render: (_, row) => (
        <Typography.Text copyable={{ text: row.roundNo }}>
          <Typography.Link
            onClick={() => history.push(`/lottery/rounds?campaignId=${row.campaignId}`)}
          >
            {row.roundNo}
          </Typography.Link>
        </Typography.Text>
      ),
    },
    {
      title: '状态',
      dataIndex: 'status',
      width: 130,
      render: (_, row) => {
        const meta = enumMeta(ROUND_STATUS, row.status);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      title: '进度',
      dataIndex: 'participantCount',
      width: 110,
      render: (_, row) => roundProgressLabel(row.participantCount, row.participantTarget),
    },
    {
      // 名额与实发是两回事：参与的人不够时全员中奖，实发会小于名额。见 winnerCountLabel。
      title: '中奖',
      dataIndex: 'winnerCount',
      width: 150,
      render: (_, row) => winnerCountLabel(row),
    },
    {
      title: '窗口',
      dataIndex: 'endsAt',
      width: 220,
      render: (_, row) => (
        <Typography.Text type="secondary">
          {time(row.startsAt)} ~ {time(row.endsAt)}
        </Typography.Text>
      ),
    },
    {
      title: '开奖时间',
      dataIndex: 'drawnAt',
      width: 170,
      render: (_, row) => time(row.drawnAt),
    },
  ];

  const renderBody = () => {
    if (error) {
      return (
        <Card>
          <Empty description={error} />
        </Card>
      );
    }
    if (!campaign) return null;
    const status = enumMeta(CAMPAIGN_STATUS, campaign.status);
    return (
      <>
        <Card title="活动" style={{ marginBottom: 16 }}>
          <Descriptions column={2} size="small">
            <Descriptions.Item label="活动名">{campaign.name}</Descriptions.Item>
            <Descriptions.Item label="短名">{campaign.code}</Descriptions.Item>
            <Descriptions.Item label="状态">
              <Tag color={status.color}>{status.text}</Tag>
            </Descriptions.Item>
            <Descriptions.Item label="是否默认">
              {/* 默认活动是开通那一刻建出来的那一个，一个开通记录只有一个。 */}
              {campaign.isDefault ? '是（开通时建的）' : '否'}
            </Descriptions.Item>
            <Descriptions.Item label="门店">{dash(campaign.locationName)}</Descriptions.Item>
            <Descriptions.Item label="适用设备">
              {/* 空 = 门店级，这是「活动再分粒度」的全部体现，所以写出来而不是留白。 */}
              {campaign.machineId ? campaign.machineId : '门店级（全门店可用）'}
            </Descriptions.Item>
            <Descriptions.Item label="参与门槛">
              {campaign.participantTarget} 人
            </Descriptions.Item>
            <Descriptions.Item label="奖池总名额">
              {campaign.prizeTotalQuantity} 个
            </Descriptions.Item>
            <Descriptions.Item label="活动窗口">
              {time(campaign.startAt)} ~ {time(campaign.endAt)}
            </Descriptions.Item>
            <Descriptions.Item label="已开期数">{campaign.roundCount} 期</Descriptions.Item>
            <Descriptions.Item label="说明" span={2}>
              {dash(campaign.description)}
            </Descriptions.Item>
          </Descriptions>
        </Card>

        <Card title="奖池" style={{ marginBottom: 16 }}>
          <Typography.Paragraph type="secondary">
            开奖时按这里的顺序依次发放名额：排在前面的档拿满自己的名额，才轮到下一档。
            所有档位的名额加起来就是每一期的中奖名额（开期时冻结到那一期上，之后改奖池不影响已开出的期次）。
          </Typography.Paragraph>
          <Table<CampaignPrize>
            rowKey="id"
            size="small"
            pagination={false}
            columns={prizeColumns}
            dataSource={campaign.prizes ?? []}
            locale={{ emptyText: <Empty description="这个活动还没有奖品" /> }}
          />
        </Card>

        <Card title="期次">
          <Table<Round>
            rowKey="id"
            size="small"
            pagination={false}
            columns={roundColumns}
            dataSource={rounds}
            locale={{ emptyText: <Empty description="还没有开过期次" /> }}
          />
        </Card>
      </>
    );
  };

  return (
    <PageContainer
      loading={loading}
      title={campaign?.name || '活动详情'}
      subTitle={campaign?.locationName || undefined}
      onBack={() => history.push('/lottery/campaigns')}
    >
      {renderBody()}
    </PageContainer>
  );
}
