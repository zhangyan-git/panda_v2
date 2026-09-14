import { PlusOutlined } from '@ant-design/icons';
import { ProTable } from '@ant-design/pro-components';
import type { ProColumns } from '@ant-design/pro-components';
import { useAccess } from '@umijs/max';
import { Avatar, Button, InputNumber, message, Switch, Tag } from 'antd';
import { useCallback, useEffect, useRef, useState } from 'react';
import {
  listDeviceDrinks,
  updateDrink,
  updateDrinkStatus,
  type Drink,
  type DrinkUpdateInput,
} from '../../../../services/coffeeMachine';
import { fenToYuan, formatYuan, yuanToFen } from '../../../../services/money';
import { requestErrorMessage } from '../../../../services/requestError';
import DrinkFormModal from '../../drinks/DrinkFormModal';

/**
 * 设备详情 → 饮品列表。
 *
 * 这一屏就是 GET /drinks 换个入口读同一张表：一行饮品自带 device_id，价格也在这行上，
 * 没有一张单独的设备×饮品关系表，所以改价与上下架走的都是「饮品自身」那两个写接口
 * （PUT /drinks/{id}、PATCH /drinks/{id}/status），不存在「改设备上的这杯」的第二条路。
 *
 * 内联改完即存：表格里没有保存按钮，失焦（价格）或切换（状态）就提交。所以每个单元格
 * 都必须自己承担「失败要退回原值」——界面停在一个没存上的数字上，比报错更难发现。
 */

/** 可内联编辑的三个价格列。单位是分，列上显示的元由渲染里换算。 */
type PriceField = 'price' | 'vipPrice' | 'pickupCodePrice';

const PRICE_FIELDS: PriceField[] = ['price', 'vipPrice', 'pickupCodePrice'];

/** 把一个价格字段写到行上。写成 switch 是为了不靠计算属性名去骗过类型系统。 */
function withPrice(row: Drink, field: PriceField, fen: number): Drink {
  if (field === 'price') return { ...row, price: fen };
  if (field === 'vipPrice') return { ...row, vipPrice: fen };
  return { ...row, pickupCodePrice: fen };
}

/** 判断这一行相对服务端值有没有真的改过。没改就不发请求。 */
function sameRow(a: Drink, b: Drink) {
  return a.sort === b.sort && PRICE_FIELDS.every((field) => a[field] === b[field]);
}

/**
 * 把列表里的一行转成编辑入参。
 *
 * PUT 是**整行覆盖**，没带的字段就是零值——只发改动的那一列，会把名称和另外两个价格
 * 一起清掉。所以每次都从这一行现取全量字段，不做「只带改动项」的优化。
 * 厂商与 originId 不在编辑入参里：那是同步的自然键。
 */
function toUpdateInput(row: Drink): DrinkUpdateInput {
  return {
    // 原样带回，别在这一屏把饮品从设备上摘下来。
    deviceId: row.deviceId,
    productNum: row.productNum,
    productName: row.productName,
    enName: row.enName,
    drinkType: row.drinkType,
    productImg: row.productImg,
    productDesc: row.productDesc,
    price: row.price,
    vipPrice: row.vipPrice,
    pickupCodePrice: row.pickupCodePrice,
    sort: row.sort,
  };
}

export default function DrinksTab({ deviceId }: { deviceId: string }) {
  const access = useAccess();
  const canWrite = access.canWriteCoffeeMachines;

  const [rows, setRows] = useState<Drink[]>([]);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState<ReadonlySet<string>>(new Set());
  const [formOpen, setFormOpen] = useState(false);

  // 行数据的真身在 ref 里，state 只是它的投影。
  //
  // 原因是失焦提交这个时序：onChange 落进 state 之后 React 要重渲染一次，blur 才带着
  // 新值进来；而 Switch 的 onChange 更直接——改完当场就要提交。从 state 闭包里读行会
  // 读到上一次渲染的旧值（改价提交了个旧价格，或者切换状态提交了个没生效的开关），
  // 而这类错法在界面上看不出来，只有落库的值是错的。
  const rowsRef = useRef<Drink[]>([]);
  // 服务端最后一次确认过的值：提交前用来判断有没有改，失败时用来退回。
  const savedRef = useRef<Map<string, Drink>>(new Map());
  // saving 的 ref 版本，给「这一行还在飞」的判重看。state 版本是给界面上的 loading 用的。
  const savingRef = useRef<Set<string>>(new Set());

  const applyRows = useCallback((next: Drink[]) => {
    rowsRef.current = next;
    setRows(next);
  }, []);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const drinks = await listDeviceDrinks(deviceId);
      savedRef.current = new Map(drinks.map((row) => [row.id, row]));
      applyRows(drinks);
    } catch (error) {
      // 设备不存在（404）和「这台设备还没有饮品」都会得到空表，但该说的话不一样，
      // 所以这里把错误报出来，而不是默默显示一张空表。
      message.error(requestErrorMessage(error, '加载设备饮品失败'));
    } finally {
      setLoading(false);
    }
  }, [deviceId, applyRows]);

  useEffect(() => {
    void load();
  }, [load]);

  const submit = async (drinkId: string) => {
    const row = rowsRef.current.find((item) => item.id === drinkId);
    const saved = savedRef.current.get(drinkId);
    if (!row || !saved) return;
    if (sameRow(row, saved)) return;
    // 同一行的一次提交还没回来就不再发第二次：回车与失焦会先后各触发一次，
    // 而第二次判「改没改」时第一次的 savedRef 还没写回去，不拦就会打两个 PUT。
    if (savingRef.current.has(drinkId)) return;

    savingRef.current.add(drinkId);
    setSaving(new Set(savingRef.current));
    try {
      await updateDrink(drinkId, toUpdateInput(row));
      savedRef.current.set(drinkId, row);
      message.success('已保存');
    } catch (error) {
      // 退回服务端值。不退回的话，界面会一直显示一个没存上的数字，而用户以为已经改好了。
      applyRows(rowsRef.current.map((item) => (item.id === drinkId ? saved : item)));
      message.error(requestErrorMessage(error, '保存失败，已退回原值'));
    } finally {
      savingRef.current.delete(drinkId);
      setSaving(new Set(savingRef.current));
    }
  };

  /** 上下架有自己的接口，不必走整行 PUT——少一次把别的字段写错的机会。 */
  const toggleStatus = async (row: Drink, onShelf: boolean) => {
    if (savingRef.current.has(row.id)) return;
    const next: Drink = { ...row, status: onShelf ? 'on_shelf' : 'off_shelf' };
    applyRows(rowsRef.current.map((item) => (item.id === row.id ? next : item)));
    savingRef.current.add(row.id);
    setSaving(new Set(savingRef.current));
    try {
      await updateDrinkStatus(row.id, next.status);
      savedRef.current.set(row.id, next);
      message.success(onShelf ? '已上架' : '已下架');
    } catch (error) {
      const saved = savedRef.current.get(row.id);
      if (saved) {
        applyRows(rowsRef.current.map((item) => (item.id === row.id ? saved : item)));
      }
      message.error(requestErrorMessage(error, '切换状态失败，已退回原值'));
    } finally {
      savingRef.current.delete(row.id);
      setSaving(new Set(savingRef.current));
    }
  };

  /** 只改本地值，不提交。价格是敲一个字符触发一次 onChange，逐键 PUT 会把中间态写进库。 */
  const patchPrice = (row: Drink, field: PriceField, fen: number) => {
    applyRows(
      rowsRef.current.map((item) => (item.id === row.id ? withPrice(item, field, fen) : item)),
    );
  };

  const priceColumn = (title: string, field: PriceField): ProColumns<Drink> => ({
    title,
    dataIndex: field,
    width: 150,
    render: (_, row) => {
      const fen = row[field];
      if (!canWrite) return `¥${formatYuan(fen)}`;
      return (
        <InputNumber
          value={fenToYuan(fen)}
          // 故意不设 min：rc-input-number 把越界值夹到 min 时不会回调 onChange，
          // 于是「敲了 -1」会变成一个界面显示 0.00、库里其实还是原价的单元格。宁可让
          // 负数走一趟服务端，由 400 明确挡下来再退回原值。
          precision={2}
          prefix="¥"
          disabled={saving.has(row.id)}
          style={{ width: 120 }}
          onChange={(value) => patchPrice(row, field, yuanToFen(value))}
          onBlur={() => void submit(row.id)}
          onPressEnter={() => void submit(row.id)}
        />
      );
    },
  });

  const columns: ProColumns<Drink>[] = [
    {
      title: '饮品图片',
      dataIndex: 'productImg',
      width: 90,
      render: (_, row) => (
        <Avatar shape="square" size={40} src={row.productImg || undefined}>
          {row.productName?.slice(0, 1)}
        </Avatar>
      ),
    },
    {
      title: '饮品名称',
      dataIndex: 'productName',
      width: 180,
      render: (_, row) => row.productName || '—',
    },
    {
      title: '编码',
      dataIndex: 'productNum',
      width: 130,
      copyable: true,
      render: (_, row) => row.productNum || '—',
    },
    priceColumn('价格', 'price'),
    priceColumn('VIP价格', 'vipPrice'),
    priceColumn('提货码价格', 'pickupCodePrice'),
    {
      title: '排序',
      dataIndex: 'sort',
      width: 70,
    },
    {
      title: '状态',
      dataIndex: 'status',
      width: 110,
      render: (_, row) =>
        canWrite ? (
          <Switch
            checked={row.status === 'on_shelf'}
            checkedChildren="上架"
            unCheckedChildren="下架"
            loading={saving.has(row.id)}
            onChange={(checked) => void toggleStatus(row, checked)}
          />
        ) : (
          <Tag color={row.status === 'on_shelf' ? 'green' : 'default'}>
            {row.status === 'on_shelf' ? '上架' : '下架'}
          </Tag>
        ),
    },
  ];

  return (
    <>
      <ProTable<Drink>
        // 标题里带一句「改价后失焦即保存」：这一屏没有保存按钮，不说的话用户会等着它出现。
        headerTitle="饮品列表（改价后失焦即保存）"
        rowKey="id"
        columns={columns}
        dataSource={rows}
        loading={loading}
        search={false}
        pagination={false}
        options={false}
        size="small"
        scroll={{ x: 1000 }}
        toolBarRender={() => [
          canWrite && (
            <Button
              key="add"
              type="primary"
              icon={<PlusOutlined />}
              onClick={() => setFormOpen(true)}
            >
              添加饮品
            </Button>
          ),
        ]}
        locale={{
          emptyText: (
            <div style={{ padding: '32px 0', color: '#999' }}>
              这台设备还没有饮品。
              <br />
              可以点右上角「添加饮品」手动加一条，也可以等厂商侧同步写入。
            </div>
          ),
        }}
      />

      <DrinkFormModal
        open={formOpen}
        onOpenChange={setFormOpen}
        editing={null}
        presetDeviceId={deviceId}
        onSaved={() => void load()}
      />
    </>
  );
}
