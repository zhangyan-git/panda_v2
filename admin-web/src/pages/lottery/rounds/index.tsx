import {
  ModalForm,
  PageContainer,
  ProFormTextArea,
  ProTable,
} from '@ant-design/pro-components';
import type { ActionType, ProColumns } from '@ant-design/pro-components';
import { useAccess, useSearchParams } from '@umijs/max';
import {
  Button,
  Descriptions,
  Empty,
  Input,
  message,
  Modal,
  Popconfirm,
  Table,
  Tag,
  Typography,
} from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { useEffect, useRef, useState } from 'react';
import { formatDateTime } from '../../../services/datetime';
import { enumMeta, searchOptions } from '../../../services/labels';
import {
  cancelRound,
  drawRound,
  getDraw,
  listCampaigns,
  listRounds,
  type Campaign,
  type DrawMode,
  type DrawResult,
  type EmptyDrawResult,
  type Round,
  type RoundQuery,
  type Win,
} from '../../../services/lottery';
import {
  DRAW_MODE,
  DRAW_TRIGGER,
  ROUND_STATUS,
  roundProgressLabel,
  winnerCountLabel,
} from '../../../services/lotteryLabels';
import { FULL_PAGE_PARAMS } from '../../../services/pagination';
import { requestErrorMessage } from '../../../services/requestError';
import { scrollableModalBody } from '../../../components/common/modalProps';

/**
 * 期次列表 + 人工开奖 / 作废。
 *
 * 期次是**滚出来的**，没有「建一期」的接口：活动一开，第一期就出来了；开了奖就在同一个
 * 事务里开下一期。所以这一页只有两个写动作，且都要 `lottery:draw` 权限（只绑 super_admin）
 * ——人工开奖是全系统少数几个能凭空决定「谁中奖」的动作。
 *
 * 开奖表单里那两个「我看到的」字段是这个设计里最要紧的一处：管理员手上那个页面可能是
 * 三十秒前加载的，这期间期次可能已经达标关闭、已经被自动开奖、甚至已经被作废。后端拿
 * 这两个值与库里的现状比对，对不上就回 409，而不是替一个已经变了的局面决定谁中奖。
 * 所以它们**必须**取自表格这一行，不能现取现填——那样这个校验就白做了。
 */

const dash = (value?: string | null) => (value ? value : '—');
const time = (value?: string | null) => (value ? formatDateTime(value) : '—');

const exact = (value: unknown): string | undefined => {
  const trimmed = typeof value === 'string' ? value.trim() : '';
  return trimmed || undefined;
};

/** 判断开奖接口走的是哪一条：有开奖记录，还是零人参与直接作废。 */
function hasDrawRecord(result: DrawResult | EmptyDrawResult): result is DrawResult {
  return result.drawId !== '';
}

export default function LotteryRoundsPage() {
  const access = useAccess();
  const actionRef = useRef<ActionType>();

  // 活动列表页的那一行「看期次」跳过来时带着 ?campaignId=xxx，这里把它当成搜索区里
  // 「活动」那个下拉的初值。
  //
  // 用 form.initialValues 而不是 ProTable 的 params 属性：params 在内部是**最后**一层
  // 合并（pageParams → formSearch → params），它会盖掉用户在下拉里改的值——那样从活动
  // 跳进来以后，这个筛选就再也换不了活动了。initialValues 只在表单初始化时生效（pro-form
  // 在挂载时用 getFieldsValue(true) 取一次，含初值），第一次请求就带上了 campaignId，
  // 之后下拉完全归用户。代价是**本页停留期间 URL 变了不会重筛**——从活动页跳过来是整页
  // 重新挂载，没有这个问题。
  const [searchParams] = useSearchParams();
  const campaignIdFromUrl = exact(searchParams.get('campaignId'));

  const [campaigns, setCampaigns] = useState<Campaign[]>([]);
  // 正在开奖的那一期。候选值是**表格里那一行**（见文件头），所以存整行而不是 id。
  const [drawing, setDrawing] = useState<Round>();
  // 正在查看的开奖记录。按需求取一次详情（列表行里只带 drawId / mode / trigger）。
  const [drawDetail, setDrawDetail] = useState<DrawResult>();

  useEffect(() => {
    void (async () => {
      try {
        const { items } = await listCampaigns(FULL_PAGE_PARAMS);
        setCampaigns(items);
      } catch {
        // 忽略：读活动要 lottery:read，能进这一页的人一定有。拿不到就只是筛选下拉是空的。
      }
    })();
  }, []);

  const campaignOptions = campaigns.map((item) => ({
    label: `${item.name}（${item.locationName}）`,
    value: item.id,
  }));

  const openDrawDetail = async (drawId: string) => {
    try {
      setDrawDetail(await getDraw(drawId));
    } catch (error) {
      message.error(requestErrorMessage(error, '加载开奖记录失败'));
    }
  };

  const columns: ProColumns<Round>[] = [
    {
      // 等值筛，不做模糊：期次号是 `{活动短名}-{四位序号}`，客服念给运营的永远是完整的一串，
      // 「ED8-0001」不该把 ED8-00010 也捞进来。发出去的键是 roundNo（见下面 request）。
      title: '期次号',
      dataIndex: 'roundNo',
      copyable: true,
      width: 150,
      fieldProps: { placeholder: '完整期次号' },
    },
    {
      // 搜索发出去的键是 campaignId（下拉选的是活动），表格里显示活动名。与其余几页同一
      // 写法。按门店看期次不在这里——那是活动列表的事。
      title: '活动',
      dataIndex: 'campaignId',
      valueType: 'select',
      valueEnum: Object.fromEntries(campaignOptions.map((o) => [o.value, { text: o.label }])),
      ellipsis: true,
      width: 160,
      render: (_, row) => dash(row.campaignName || row.campaignCode),
    },
    {
      title: '状态',
      dataIndex: 'status',
      valueType: 'select',
      valueEnum: searchOptions(ROUND_STATUS),
      width: 130,
      render: (_, row) => {
        const meta = enumMeta(ROUND_STATUS, row.status);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      title: '进度',
      dataIndex: 'participantCount',
      search: false,
      width: 120,
      render: (_, row) => roundProgressLabel(row.participantCount, row.participantTarget),
    },
    {
      // 名额与实发分开显示：参与的人不够时全员中奖，实发会小于名额（剩下的名额流掉，
      // 那不是异常）。把名额当结果显示会让人以为没开奖成功。
      title: '中奖',
      dataIndex: 'winnerCount',
      search: false,
      width: 150,
      render: (_, row) => winnerCountLabel(row),
    },
    {
      // 「这一期为什么开了」的答案在这一列：threshold 是参与**次数**收满了门槛（在确认那
      // 一刻把期次置 closed，worker 随后开掉），manual 是管理员直接开的。这里原本还有一档
      // 「到点」，2026-09-15 随期次窗口一起删了。
      title: '开奖方式',
      dataIndex: 'drawMode',
      search: false,
      width: 110,
      render: (_, row) => {
        if (!row.drawMode) return '—';
        const mode = enumMeta(DRAW_MODE, row.drawMode);
        return <Tag color={mode.color}>{mode.text}</Tag>;
      },
    },
    {
      title: '开奖时间',
      dataIndex: 'drawnAt',
      search: false,
      width: 170,
      render: (_, row) => time(row.drawnAt),
    },
    {
      title: '操作',
      valueType: 'option',
      // fixed 的列必须显式给宽度，且下面的 scroll.x 必须等于各列宽度之和（见下面的 1180）。
      // 190 是量出来的：三个 link 按钮（看开奖 / 开奖 / 作废）并排内容宽约 174，加左右各
      // 8px 内边距。这一列**只有部分行**会渲染满三个按钮，但宽度必须按满的情况留——
      // 钉右列的溢出会把表格 scrollWidth 顶大，与 scroll.x 对不上。
      width: 190,
      fixed: 'right',
      render: (_, row) => {
        const actions: React.ReactNode[] = [];
        if (row.drawId) {
          actions.push(
            <Button
              key="view"
              type="link"
              size="small"
              onClick={() => void openDrawDetail(row.drawId)}
            >
              看开奖
            </Button>,
          );
        }
        // 只有能开奖的期次才给开奖按钮：open（还没收满，但可以人工提前开）与 closed
        // （已收满待开）。只让这一条路出现，是为了不把 409 做成一个能点到的按钮。
        //
        // open 这一档是**收不满时的唯一出路**：没有到点必开之后，一个没收满的期次会一直
        // 开着，运营只能人工开掉或者作废它。
        if (access.canDrawLottery && (row.status === 'open' || row.status === 'closed')) {
          actions.push(
            <Button key="draw" type="link" size="small" onClick={() => setDrawing(row)}>
              开奖
            </Button>,
          );
        }
        // 作废**只给零人参与的期次**：后端有参与者的期次一律拒绝（那需要沿 N 次跨服务
        // 冲正把卡还回去，属下一轮）。别把按钮留给注定 409 的情况。
        if (access.canDrawLottery && row.participantCount === 0 && row.status !== 'drawn' && row.status !== 'cancelled') {
          actions.push(
            <CancelRoundButton key="cancel" round={row} onDone={() => actionRef.current?.reload()} />,
          );
        }
        return actions;
      },
    },
  ];

  // 这张表是 antd 的 Table（不是 ProTable），列类型 ColumnsType **不认** ProColumns 的
  // copyable。凭证号与用户 ID 都要能复制（客服把这两个号念给用户是这一页的日常），
  // 所以自己包一层 Typography.Text。
  const winnerColumns: ColumnsType<Win> = [
    {
      title: '凭证号',
      dataIndex: 'claimNo',
      width: 170,
      render: (_, row) => <Typography.Text copyable={{ text: row.claimNo }}>{row.claimNo}</Typography.Text>,
    },
    {
      title: '用户',
      dataIndex: 'userId',
      width: 280,
      render: (_, row) => (
        <Typography.Text copyable={{ text: row.userId }} ellipsis>
          {row.userId}
        </Typography.Text>
      ),
    },
    {
      // 这里原先还有一个奖品类型的 Tag（PRIZE_KIND）。类型那一列 2026-09-15 删了——它从
      // 落地起就只存不消费，中奖记录上那份快照也跟着一起没了。
      title: '奖品',
      dataIndex: 'prizeId',
      width: 200,
      ellipsis: true,
      render: (_, row) => row.currentPrizeName,
    },
  ];

  return (
    <PageContainer
      title="期次"
      content="期次由活动滚动产生：收满门槛就自动开奖，开奖的那一刻同一事务里开出下一期。没满就一直开着等，不会有东西因为时间到了把它开掉——收不满只能人工开奖或作废。人工开奖与作废只绑给超级管理员。"
    >
      <ProTable<Round>
        actionRef={actionRef}
        rowKey="id"
        columns={columns}
        // 1180 = 150+160+130+120+150+110+170+190，各列 width 之和（原「窗口」列的 220
        // 随窗口一起删了）。
        scroll={{ x: 1180 }}
        search={{ labelWidth: 'auto' }}
        // 从活动页跳过来时预选那一个活动（见上面 campaignIdFromUrl 那段）。
        form={{ initialValues: { campaignId: campaignIdFromUrl } }}
        options={false}
        request={async (params) => {
          const query: RoundQuery = {
            page: params.current,
            pageSize: params.pageSize,
            // 期次号是等值筛（后端 `r.round_no = $n`）。原先这个键根本没往下传，框填了等于
            // 没填——列表原样返回，也不报错。
            roundNo: exact(params.roundNo),
            campaignId: exact(params.campaignId),
            status: exact(params.status) as RoundQuery['status'],
          };
          const result = await listRounds(query);
          return { data: result.items, total: result.total, success: true };
        }}
      />

      {/* 人工开奖。理由必填——事后要能回答「当时为什么提前开了」这一期。 */}
      <ModalForm<{ reason: string }>
        title={drawing ? `开奖：${drawing.roundNo}` : '开奖'}
        open={!!drawing}
        onOpenChange={(open) => {
          if (!open) setDrawing(undefined);
        }}
        modalProps={{ ...scrollableModalBody, destroyOnClose: true }}
        onFinish={async (values) => {
          if (!drawing) return true;
          try {
            const result = await drawRound(drawing.id, {
              reason: values.reason.trim(),
              // 这两个值取自**页面上这一行**，不是现取现填——它们就是后端用来判断
              // 「你看的是不是当前局面」的依据（见文件头）。
              expectedRoundStatus: drawing.status,
              expectedParticipantCount: drawing.participantCount,
            });
            if (hasDrawRecord(result)) {
              message.success(
                `已开奖，中奖 ${result.winnerCount} 人${
                  result.nextRoundNo ? `，下一期 ${result.nextRoundNo} 已开出` : ''
                }`,
              );
            } else {
              message.info(result.message || '本期无人参与，已作废');
            }
          } catch (error) {
            message.error(requestErrorMessage(error, '开奖失败，请稍后重试'));
            // 留在原地：409 的时候人需要看到那句话，而且刷新页面拿到新状态比重新填一遍
            // 理由更重要。
            return false;
          }
          // 重取一次：这一期变成已开奖，下一期已经出来了，列表整个变了。
          actionRef.current?.reload();
          return true;
        }}
      >
        <Typography.Paragraph type="secondary">
          开奖会按记录在案的种子与参与集合算出中奖名单，<strong>一旦开出不可重抽</strong>
          （本轮没有重抽）。这一下会写进只增的开奖记录与平台操作日志。
        </Typography.Paragraph>
        {drawing ? (
          <Descriptions column={2} size="small" style={{ marginBottom: 16 }}>
            <Descriptions.Item label="当前状态">
              {enumMeta(ROUND_STATUS, drawing.status).text}
            </Descriptions.Item>
            <Descriptions.Item label="已参与">
              {roundProgressLabel(drawing.participantCount, drawing.participantTarget)}
            </Descriptions.Item>
            <Descriptions.Item label="中奖名额" span={2}>
              {drawing.winnerCount} 个
            </Descriptions.Item>
          </Descriptions>
        ) : null}
        <ProFormTextArea
          name="reason"
          label="开奖理由"
          placeholder="会写进开奖记录与操作日志，请说明依据（如：活动提前结束，人工结期中）"
          fieldProps={{ rows: 3, maxLength: 200, showCount: true }}
          rules={[{ required: true, message: '请输入开奖理由' }]}
        />
      </ModalForm>

      {/* 一次开奖的记录。种子摆出来是为了能复核：拿它加参与集合可以把名单完整重算一遍。 */}
      <Modal
        title={drawDetail ? `开奖记录 ${drawDetail.roundNo}` : '开奖记录'}
        open={!!drawDetail}
        onCancel={() => setDrawDetail(undefined)}
        footer={null}
        width={900}
      >
        {drawDetail ? (
          <>
            <Descriptions column={2} size="small" style={{ marginBottom: 16 }}>
              <Descriptions.Item label="方式">
                {enumMeta(DRAW_MODE, drawDetail.mode).text}
              </Descriptions.Item>
              <Descriptions.Item label="触发">
                {enumMeta(DRAW_TRIGGER, drawDetail.trigger).text}
              </Descriptions.Item>
              <Descriptions.Item label="参与次数">
                {drawDetail.participantCount}
              </Descriptions.Item>
              <Descriptions.Item label="中奖人数">{drawDetail.winnerCount}</Descriptions.Item>
              <Descriptions.Item label="开奖时间">{time(drawDetail.createdAt)}</Descriptions.Item>
              <Descriptions.Item label="算法">{drawDetail.algorithm}</Descriptions.Item>
              <Descriptions.Item label="种子" span={2}>
                <Typography.Text code copyable>
                  {drawDetail.seed}
                </Typography.Text>
              </Descriptions.Item>
            </Descriptions>
            <Table<Win>
              rowKey="id"
              size="small"
              pagination={false}
              columns={winnerColumns}
              dataSource={drawDetail.winners ?? []}
              locale={{ emptyText: <Empty description="这份开奖记录里没有中奖人" /> }}
            />
          </>
        ) : null}
      </Modal>
    </PageContainer>
  );
}

/**
 * 作废一期。抽成小组件只为一件事：它自己有 loading 状态，而按钮是列表里的一格——
 * 挂一个 state 到列表页上会让整张表跟着重渲染。
 */
function CancelRoundButton({ round, onDone }: { round: Round; onDone: () => void }) {
  const [busy, setBusy] = useState(false);

  const cancel = async (reason: string) => {
    setBusy(true);
    try {
      await cancelRound(round.id, reason);
      message.success('已作废');
      onDone();
    } catch (error) {
      message.error(requestErrorMessage(error, '作废失败，请稍后重试'));
    } finally {
      setBusy(false);
    }
  };

  // 这里用一个自带输入框的 Popconfirm：作废同样要留下理由，但它的分量比开奖轻（这一期
  // 一个人都没有），所以不值得为它开一个弹窗表单。
  //
  // 输入框用 antd 的 Input.TextArea 而**不是 ProFormTextArea**：后者是 ProForm 的字段
  // 组件，靠 Form.Item 注入 value / onChange，摆在一个 Popconfirm 里没有那个上下文，
  // 写进去的字根本不会流出来。
  const [reason, setReason] = useState('');
  return (
    <Popconfirm
      title="作废这一期？"
      icon={null}
      description={
        <div style={{ width: 260 }}>
          <Typography.Paragraph type="secondary" style={{ marginBottom: 8 }}>
            这一期还没有人参与，作废不需要退卡。作废后会立刻开出下一期。
          </Typography.Paragraph>
          <Input.TextArea
            rows={2}
            maxLength={200}
            placeholder="作废理由，会写进记录"
            value={reason}
            onChange={(event) => setReason(event.target.value)}
          />
        </div>
      }
      okText="作废"
      cancelText="取消"
      okButtonProps={{ danger: true, loading: busy, disabled: !reason.trim() }}
      onConfirm={() => cancel(reason.trim())}
    >
      <Button type="link" size="small" danger>
        作废
      </Button>
    </Popconfirm>
  );
}
