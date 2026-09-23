import {
  ModalForm,
  PageContainer,
  ProFormDigit,
  ProFormSelect,
  ProFormText,
  ProFormTextArea,
  ProTable,
} from '@ant-design/pro-components';
import type { ActionType, ProColumns } from '@ant-design/pro-components';
import { ProFormImageUpload } from '@panda-v2/ui';
import { history, useAccess } from '@umijs/max';
import { Button, message, Popconfirm, Tag, Typography } from 'antd';
import { useEffect, useRef, useState } from 'react';
import { listDeviceOptions, type DeviceSummary } from '../../../services/coffeeMachine';
import { enumMeta, searchOptions } from '../../../services/labels';
import {
  createCampaign,
  getCampaign,
  listActivations,
  listCampaigns,
  updateCampaign,
  updateCampaignStatus,
  type Activation,
  type Campaign,
  type CampaignInput,
  type CampaignQuery,
  type CampaignStatus,
  type PrizeInput,
} from '../../../services/lottery';
import { CAMPAIGN_STATUS, roundProgressLabel } from '../../../services/lotteryLabels';
import { FULL_PAGE_PARAMS } from '../../../services/pagination';
import { requestErrorMessage } from '../../../services/requestError';
import { uploadImage } from '../../../services/upload';
import { scrollableModalBody } from '../../../components/common/modalProps';

/**
 * 抽奖活动列表。
 *
 * 「开通挂门店，活动再分粒度」——所以一个活动的归属由**两段**决定：它属于哪条开通记录
 * （也就是哪家门店），以及它是不是只对**该门店下某一台咖啡机**开放。后者落在可空的
 * machineId 上：为空就是门店级，有值就是那台设备专属。
 *
 * 结构上不写成商户账号那种多态范围（档位 + 一组目标引用），是为了让「某台咖啡机的活动不属于本门店」这种
 * 组合**写不出来**——门店来自 activation，设备只能在这条线之下。
 *
 * 奖品是活动的一部分，跟着活动一起提交。**一个活动只有一个奖品**——这里原先是一张可增删
 * 的奖品行列表（每行带类型与名额），2026-09-15 收敛成一个：运营侧实际就是一个活动一个
 * 奖品，名额恒为 1。
 */

/** 接口给的空值一律显示成「—」：留白和「有个空字符串」在表里分不出来。 */
const dash = (value?: string | null) => (value ? value : '—');

/** 精确匹配的筛选参数：发出去之前 trim，空的整个丢掉（后端是等值比较，不是模糊）。 */
const exact = (value: unknown): string | undefined => {
  const trimmed = typeof value === 'string' ? value.trim() : '';
  return trimmed || undefined;
};

/**
 * 活动表单。奖品是四个平铺字段，用 `['prize', x]` 这样的嵌套名收成一个对象。
 *
 * **表单里没有奖品的 id**，所以提交时要显式带上（见 onFinish）。带上它是必须的：奖品行被
 * 中奖记录引用着，不带 id 服务端会把它删掉重插，而那次删除会被外键拒绝、保存直接 500。
 */
type CampaignForm = {
  activationId: string;
  machineId?: string;
  code: string;
  name: string;
  participantTarget: number;
  description?: string;
  status: CampaignStatus;
  prize: Omit<PrizeInput, 'id'>;
};

/** 新建时的空奖品：把字段摆全，免得四个框里有两个是 undefined。 */
const EMPTY_PRIZE: CampaignForm['prize'] = {
  name: '',
  coverImage: '',
  posterImage: '',
  claimInstructions: '',
};

export default function LotteryCampaignsPage() {
  const access = useAccess();
  const actionRef = useRef<ActionType>();
  const [activations, setActivations] = useState<Activation[]>([]);
  const [devices, setDevices] = useState<DeviceSummary[]>([]);
  const [open, setOpen] = useState(false);
  // 正在编辑的活动**详情**（带奖品）。列表那一行不带 prize，所以编辑必须先把详情取回来
  // ——否则保存时提交的奖品是空的，等于把奖品清掉了。它还是奖品 id 的来源（见 onFinish）。
  const [editing, setEditing] = useState<Campaign>();
  const [loadingDetail, setLoadingDetail] = useState(false);

  useEffect(() => {
    void (async () => {
      try {
        const { items } = await listActivations(FULL_PAGE_PARAMS);
        setActivations(items);
      } catch {
        // 忽略：读开通记录要 lottery:read，能进这一页的人一定有；真失败了表单里的门店
        // 下拉就是空的，运营看得出「选不了店」而不是被静默放进一个错的活动。
      }
    })();
    void (async () => {
      try {
        // 设备名要从咖啡机域现取，所以这一次请求会多要一个 coffee_machine:read——没有它
        // 就失败并退回显示 id（下面 render 的兜底），不该因此让整页报错。与饮品管理页
        // 同一取舍。
        setDevices(await listDeviceOptions());
      } catch {
        setDevices([]);
      }
    })();
  }, []);

  const activationOptions = activations.map((item) => ({
    label: item.locationName || item.id,
    value: item.id,
  }));

  /**
   * 筛选用的门店下拉。**复用上面那份开通记录**，不再拉一次门店列表：这一页的「门店」就是
   * 「开通过抽奖的门店」，没开通的店本来也没有活动可筛。顺带少要一个 admin:stores:view
   * 权限——只有抽奖权限的人也该筛得了这一列。
   *
   * 键是 locationId（筛选发出去的就是它），标签是读这一刻现解出来的店名。
   */
  const storeOptions = Array.from(
    new Map(activations.map((item) => [item.locationId, item.locationName || item.locationId])),
  ).map(([value, label]) => ({ value, label }));

  /** 设备 id → 「设备名（序列号）」。取不到（没权限 / 超出下拉上限）时下面退回显示 id。 */
  const deviceNames = Object.fromEntries(
    devices.map((device) => [
      device.id,
      `${device.deviceName || '未命名设备'}（${device.serialUnique}）`,
    ]),
  );

  /**
   * 打开编辑。**先取详情再开弹窗**：弹窗一开就得把奖品填进去，而列表行里没有它。
   *
   * 取不到就报错并**不开弹窗**——开一个奖品空着的编辑框，运营点保存就把奖品清掉了。
   */
  const openEdit = async (id: string) => {
    setLoadingDetail(true);
    try {
      setEditing(await getCampaign(id));
      setOpen(true);
    } catch (error) {
      message.error(requestErrorMessage(error, '加载活动详情失败'));
    } finally {
      setLoadingDetail(false);
    }
  };

  const setStatus = async (row: Campaign, status: CampaignStatus) => {
    try {
      await updateCampaignStatus(row.id, status);
      message.success('已更新');
      actionRef.current?.reload();
    } catch (error) {
      message.error(requestErrorMessage(error, '操作失败，请稍后重试'));
    }
  };

  const columns: ProColumns<Campaign>[] = [
    {
      title: '活动名',
      dataIndex: 'name',
      ellipsis: true,
      width: 160,
      fieldProps: { placeholder: '活动名关键词' },
      render: (_, row) => (
        <Typography.Link onClick={() => history.push(`/lottery/campaigns/${row.id}`)}>
          {row.name}
        </Typography.Link>
      ),
    },
    {
      // 短名是期次号的前缀（`{code}-{seq:04d}`），建出来就不可改：已经开出去的期次号里
      // 嵌着它，改一个字母会让历史期次号与活动对不上。后端 CampaignQuery 也没有按它筛的
      // 能力，所以这一列纯展示。
      title: '短名',
      dataIndex: 'code',
      search: false,
      copyable: true,
      width: 120,
    },
    {
      // 筛选是**从门店下拉里选一家**，传 locationId 走后端那条等值比较；表格里显示接口
      // 自带的 locationName。与开通页同一写法。
      //
      // 这里原先是一个「填完整门店 ID」的输入框，而那等于筛不了：uuid 没人背得出来，复制
      // 还得先有个地方能拿到它。名字不落库（migrations/lottery），SQL 里没有一列能做
      // ILIKE，商户域的 gRPC 也只有 ListStores（只收 merchantId）和 ResolveScopeNames
      // （只收 id）——所以这一列能筛的只有 id，而 id 只能从下拉里选。
      title: '门店',
      dataIndex: 'locationId',
      valueType: 'select',
      ellipsis: true,
      width: 160,
      fieldProps: {
        options: storeOptions,
        showSearch: true,
        // 按标签（店名）过滤，而不是按 value（uuid）——照 uuid 搜等于没得搜。
        optionFilterProp: 'label',
        // 开通记录读不到时是空下拉，那是**看得出来的降级**，不是静默筛不到。
        placeholder: storeOptions.length ? '选择门店' : '开通记录读不到，筛不了门店',
      },
      render: (_, row) => dash(row.locationName),
    },
    {
      // 空 = 门店级。这一列是「活动分粒度」在界面上的全部体现，所以「门店级」三个字要
      // 写出来而不是留白——留白会让人以为这一格没数据。
      //
      // 筛选与门店那一列同一个毛病：原先也要人填完整设备 ID，而设备 id 比门店 id 更没人
      // 背得出来。设备名在咖啡机域，本库不留，所以同样只有下拉这一条路（listDeviceOptions）。
      title: '适用设备',
      dataIndex: 'machineId',
      valueType: 'select',
      ellipsis: true,
      width: 160,
      // valueEnum 同时供给筛选下拉与单元格文案；单元格另有 render 兜底，见下面。
      valueEnum: Object.fromEntries(
        devices.map((device) => [
          device.id,
          { text: `${device.deviceName || '未命名设备'}（${device.serialUnique}）` },
        ]),
      ),
      fieldProps: {
        showSearch: true,
        // 按标签（设备名）过滤，而不是按 value（uuid）。
        optionFilterProp: 'label',
        placeholder: devices.length ? '选择设备' : '设备列表读不到，筛不了设备',
      },
      // 设备没取到时退回显示 id：一条设备级活动显示「门店级」是错的，显示 uuid 至少能认。
      render: (_, row) =>
        row.machineId ? (deviceNames[row.machineId] ?? row.machineId) : '门店级',
    },
    {
      title: '状态',
      dataIndex: 'status',
      valueType: 'select',
      valueEnum: searchOptions(CAMPAIGN_STATUS),
      width: 90,
      render: (_, row) => {
        const meta = enumMeta(CAMPAIGN_STATUS, row.status);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      title: '参与门槛',
      dataIndex: 'participantTarget',
      search: false,
      width: 100,
      render: (_, row) => `${row.participantTarget} 次`,
    },
    // 这里原先还有一列「奖池名额」（prizeTotalQuantity = SUM(prizes.quantity)）。名额恒为 1
    // 之后这一列每一行都是「1 个」，不再有任何信息；真要核名额看「进行中的期次」那一期的
    // winnerCount，那是开期时冻结下来的真值。2026-09-15 删掉。
    {
      title: '进行中的期次',
      dataIndex: 'liveRoundNo',
      search: false,
      width: 170,
      render: (_, row) =>
        row.liveRoundId ? (
          <span>
            {row.liveRoundNo}
            <Typography.Text type="secondary">
              {' '}
              {roundProgressLabel(row.liveRoundDone, row.liveRoundSize)}
            </Typography.Text>
          </span>
        ) : (
          '—'
        ),
    },
    {
      title: '已开期数',
      dataIndex: 'roundCount',
      search: false,
      width: 100,
      render: (_, row) => `${row.roundCount} 期`,
    },
    {
      title: '操作',
      valueType: 'option',
      // fixed 的列必须显式给宽度，且下面的 scroll.x 必须等于各列宽度之和（见下面的 1250）。
      // 190 是量出来的：三个 link 按钮（详情 / 编辑 / 暂停）并排内容宽约 174，加左右各
      // 8px 内边距。**钉右列宽度给窄了的后果是表格 scrollWidth 被顶大**，与 scroll.x
      // 声明的数对不上（这个仓库踩过）。
      width: 190,
      fixed: 'right',
      render: (_, row) => {
        const actions: React.ReactNode[] = [
          <Button
            key="detail"
            type="link"
            size="small"
            onClick={() => history.push(`/lottery/campaigns/${row.id}`)}
          >
            详情
          </Button>,
        ];
        if (access.canManageLottery) {
          actions.push(
            <Button key="edit" type="link" size="small" loading={loadingDetail} onClick={() => void openEdit(row.id)}>
              编辑
            </Button>,
          );
          // 只给「进行中」的提供暂停、给「已暂停」的提供恢复。草稿与已结束不在这里动：
          // 草稿要先编辑成启用，已结束是终态。
          if (row.status === 'enabled') {
            actions.push(
              <Popconfirm
                key="pause"
                title="暂停这个活动？"
                description="暂停后不再开新期，正在跑的这一期会走完并正常开奖。已开出的中奖记录不受影响。"
                okText="暂停"
                cancelText="取消"
                onConfirm={() => setStatus(row, 'paused')}
              >
                <Button type="link" size="small" danger>
                  暂停
                </Button>
              </Popconfirm>,
            );
          } else if (row.status === 'paused') {
            actions.push(
              <Button key="resume" type="link" size="small" onClick={() => setStatus(row, 'enabled')}>
                恢复
              </Button>,
            );
          }
        }
        return actions;
      },
    },
  ];

  return (
    <PageContainer
      title="抽奖活动"
      content="活动挂在门店下，可以只对某台咖啡机开放。每个活动带一个奖品，每一期开出一名中奖者。"
      extra={
        access.canManageLottery
          ? [
              <Button
                key="create"
                type="primary"
                onClick={() => {
                  setEditing(undefined);
                  setOpen(true);
                }}
              >
                新建活动
              </Button>,
            ]
          : undefined
      }
    >
      <ProTable<Campaign>
        actionRef={actionRef}
        rowKey="id"
        columns={columns}
        // 1250 = 160+120+160+160+90+100+170+100+190，各列 width 之和（原「活动窗口」列的
        // 200 随窗口一起删了，「奖池名额」列的 100 随名额恒为 1 一起删了）。**钉右列不变量**：
        // 下面「操作」列的 width 与这里必须同批改，对不上表格就会横向溢出。
        scroll={{ x: 1250 }}
        search={{ labelWidth: 'auto' }}
        options={false}
        request={async (params) => {
          const query: CampaignQuery = {
            page: params.current,
            pageSize: params.pageSize,
            locationId: exact(params.locationId),
            machineId: exact(params.machineId),
            status: exact(params.status) as CampaignQuery['status'],
            name: exact(params.name),
          };
          const result = await listCampaigns(query);
          return { data: result.items, total: result.total, success: true };
        }}
      />

      {/*
        **destroyOnClose 是必须的**：上一次编辑留下的奖品会留在表单里，下一次新建时会带着
        别人的奖品一起提交。

        key 跟着编辑对象走，两重保险：换一个活动编辑时表单整个重建，不会残留上一份奖品。
      */}
      <ModalForm<CampaignForm>
        key={editing?.id ?? 'new'}
        title={editing ? `编辑活动 ${editing.name}` : '新建抽奖活动'}
        open={open}
        onOpenChange={(value) => {
          setOpen(value);
          if (!value) setEditing(undefined);
        }}
        modalProps={{ ...scrollableModalBody, destroyOnClose: true, width: 760 }}
        initialValues={
          editing
            ? {
                activationId: editing.activationId,
                machineId: editing.machineId ?? undefined,
                code: editing.code,
                name: editing.name,
                participantTarget: editing.participantTarget,
                description: editing.description,
                status: editing.status,
                prize: {
                  name: editing.prize?.name ?? '',
                  coverImage: editing.prize?.coverImage ?? '',
                  posterImage: editing.prize?.posterImage ?? '',
                  claimInstructions: editing.prize?.claimInstructions ?? '',
                },
              }
            : {
                status: 'draft' as CampaignStatus,
                participantTarget: 10,
                prize: EMPTY_PRIZE,
              }
        }
        onFinish={async (values) => {
          const payload: CampaignInput = {
            activationId: values.activationId,
            machineId: values.machineId?.trim() || null,
            code: values.code.trim().toUpperCase(),
            name: values.name.trim(),
            participantTarget: values.participantTarget,
            description: values.description?.trim(),
            status: values.status,
            prize: {
              // **id 从 editing 上取，不从表单取**：表单里没有这一格，放一个没有输入框的
              // 隐藏字段只在 initialValues 里出现过一次，谁也不会发现它丢了——而丢了就是
              // 保存 500（见上面 CampaignForm 的说明）。写成一行看得见的分支，改的人知道它
              // 为什么在这儿。
              id: editing?.prize?.id,
              name: values.prize.name.trim(),
              coverImage: values.prize.coverImage?.trim() ?? '',
              posterImage: values.prize.posterImage?.trim() ?? '',
              claimInstructions: values.prize.claimInstructions?.trim() ?? '',
            },
          };
          try {
            if (editing) {
              await updateCampaign(editing.id, payload);
            } else {
              await createCampaign(payload);
            }
          } catch (error) {
            message.error(requestErrorMessage(error, '保存失败，请稍后重试'));
            return false;
          }
          message.success(editing ? '已保存' : '已创建');
          actionRef.current?.reload();
          return true;
        }}
      >
        <ProFormSelect
          name="activationId"
          label="所属门店"
          options={activationOptions}
          showSearch
          // 编辑时禁用：活动不能换门店（换了等于新建一个），后端也整个忽略这个字段。
          disabled={!!editing}
          rules={[{ required: true, message: '请选择门店' }]}
          fieldProps={{
            filterOption: (input: string, option) =>
              String(option?.label ?? '')
                .toLowerCase()
                .includes(input.toLowerCase()),
          }}
        />
        {/*
          这里原来是一个「填完整设备 ID」的输入框，理由是「为一格大多数活动都不填的字段
          拉整份设备表不划算」。那个理由现在不成立了：设备表为了上面那一列的筛选已经拉了，
          而一个要人先想办法拿到 uuid 的输入框，实际等于选不了设备。
        */}
        <ProFormSelect
          name="machineId"
          label="适用设备"
          placeholder="留空 = 全门店可用"
          allowClear
          showSearch
          options={devices.map((device) => ({
            label: deviceNames[device.id],
            value: device.id,
          }))}
          fieldProps={{ optionFilterProp: 'label' }}
          tooltip="选了就是设备级活动：只有从这台咖啡机扫进来的参与算数。留空是门店级。"
        />
        <ProFormText
          name="code"
          label="活动短名"
          rules={[
            { required: true, message: '请输入活动短名' },
            // 与后端同一条规则：1-16 位大写字母或数字。它会是期次号的前缀（如 LAKE-0001），
            // 所以不接受小写与空格——那些会让期次号出现两种写法。
            { pattern: /^[A-Za-z0-9]{1,16}$/, message: '1-16 位字母或数字，不能有空格与符号' },
          ]}
          fieldProps={{ placeholder: '如 LAKE，会变成期次号 LAKE-0001' }}
          tooltip="建出来之后不可改：已经开出去的期次号里嵌着它。"
        />
        <ProFormText
          name="name"
          label="活动名"
          rules={[{ required: true, message: '请输入活动名' }]}
          fieldProps={{ maxLength: 50 }}
        />
        <ProFormDigit
          name="participantTarget"
          label="参与门槛"
          tooltip="每一期收满这么多次参与就停止收人并开奖；同一个人可以参与多次。开期时会冻结到那一期上，所以改它只影响之后的期次。"
          min={1}
          max={100000}
          rules={[{ required: true, message: '请输入参与门槛' }]}
          fieldProps={{ precision: 0 }}
        />
        <ProFormSelect
          name="status"
          label="状态"
          options={[
            { label: '草稿（不开期）', value: 'draft' },
            { label: '进行中（开始开期收人）', value: 'enabled' },
            { label: '已暂停', value: 'paused' },
            { label: '已结束', value: 'ended' },
          ]}
          rules={[{ required: true, message: '请选择状态' }]}
        />
        <ProFormTextArea
          name="description"
          label="活动说明"
          placeholder="会显示在小程序抽奖中心，可留空"
          fieldProps={{ rows: 2, maxLength: 200 }}
        />

        {/*
          奖品。**一个活动一个**：这里原先是可增删的奖品行列表（每行带类型、名称、名额），
          2026-09-15 收敛成四个平铺字段。类型与名额两格删了——类型从落地起只存不消费，
          名额恒为 1（看名额的地方是期次上的 winnerCount）。
        */}
        <ProFormText
          name={['prize', 'name']}
          label="奖品名称"
          rules={[{ required: true, message: '请输入奖品名称' }]}
          fieldProps={{ maxLength: 50, placeholder: '如 10 元咖啡兑换券' }}
        />
        {/*
          两张图并排，照品牌页 Logo / Banner 那一对的写法。

          **封面图必填**（服务端也校验，那条才是真闸门）：活动列表与卡片上要显示它，留空会
          让卡片缺一块。海报图可留空，为空时前端回落到自带的那块占位。
          两张图都**不校验比例**——比例还没定，定了之后要改的是这里的 tooltip 与服务端校验，
          不是这个组件（它没有裁剪能力，这次也不给它加）。
        */}
        <ProFormImageUpload
          name={['prize', 'coverImage']}
          label="封面图"
          colProps={{ span: 12 }}
          upload={uploadImage}
          rules={[{ required: true, message: '请上传奖品封面图' }]}
          tooltip="用在活动卡片上。"
        />
        <ProFormImageUpload
          name={['prize', 'posterImage']}
          label="海报图"
          colProps={{ span: 12 }}
          upload={uploadImage}
          tooltip="用在活动详情顶部的横幅，可留空。"
        />
        <ProFormTextArea
          name={['prize', 'claimInstructions']}
          label="领取说明"
          placeholder="显示在中奖详情页，如 到门店出示凭证号即可"
          fieldProps={{ rows: 2, maxLength: 100 }}
        />
      </ModalForm>
    </PageContainer>
  );
}
