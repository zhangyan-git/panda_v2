import { PageContainer } from '@ant-design/pro-components';
import { history, useParams } from '@umijs/max';
import { Card, Descriptions, Empty, Image, Table, Tag, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { useCallback, useEffect, useState } from 'react';
import { formatDateTime } from '../../../../services/datetime';
import { enumMeta } from '../../../../services/labels';
import { getCampaign, listRounds, type Campaign, type Round } from '../../../../services/lottery';
import {
  CAMPAIGN_STATUS,
  ROUND_STATUS,
  roundProgressLabel,
  winnerCountLabel,
} from '../../../../services/lotteryLabels';
import { FULL_PAGE_PARAMS } from '../../../../services/pagination';
import { requestErrorMessage } from '../../../../services/requestError';

/**
 * 活动详情：活动本身 + 它的奖品 + 它开过的期次。
 *
 * **奖品只有这一条路能看到**：列表接口有意不带它（一次 20 个活动、每个带两张图，列表响应
 * 会白胖一圈），所以想看这个活动发的是什么必须进这一页。
 *
 * 期次是滚出来的，没有「建一期」的接口，所以这一页在这一块**只读**：能动的两个动作
 * （开奖 / 作废）在期次列表页上，因为它们作用于某**一期**而不是整个活动。
 */

const dash = (value?: string | null) => (value ? value : '—');
const time = (value?: string | null) => (value ? formatDateTime(value) : '—');

/**
 * 一张图，空值时写「未设置」而不是留白——**开通模板建出来的奖品封面就是空的**
 * （开通是一键动作，不该被「先找一张图」挡住），所以这不是异常局面，得看得出来。
 */
const imageOrNone = (src: string) =>
  src ? (
    <Image src={src} width={160} />
  ) : (
    <Typography.Text type="secondary">未设置</Typography.Text>
  );

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

  // 奖品原先是一张表（顺序 / 类型 / 名称 / 名额 / 领取说明）。一个活动一个奖品之后表格没了
  // ——一行的表格不是表格，是一块 Descriptions。

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
              {campaign.participantTarget} 次
            </Descriptions.Item>
            <Descriptions.Item label="已开期数">{campaign.roundCount} 期</Descriptions.Item>
            <Descriptions.Item label="说明" span={2}>
              {dash(campaign.description)}
            </Descriptions.Item>
          </Descriptions>
        </Card>

        <Card title="奖品" style={{ marginBottom: 16 }}>
          <Typography.Paragraph type="secondary">
            每一期开出一名中奖者，发的就是这个奖品。改这里只影响之后的期次——奖品名在开奖时
            就快照进了中奖记录。
          </Typography.Paragraph>
          {campaign.prize ? (
            <Descriptions column={2} size="small">
              <Descriptions.Item label="奖品名称" span={2}>
                {campaign.prize.name}
              </Descriptions.Item>
              <Descriptions.Item label="领取说明" span={2}>
                {dash(campaign.prize.claimInstructions)}
              </Descriptions.Item>
              <Descriptions.Item label="封面图">
                {imageOrNone(campaign.prize.coverImage)}
              </Descriptions.Item>
              <Descriptions.Item label="海报图">
                {imageOrNone(campaign.prize.posterImage)}
              </Descriptions.Item>
            </Descriptions>
          ) : (
            <Empty description="这个活动还没有奖品" />
          )}
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
