import { listBrands } from '../../services/brand';
import { listDeviceOptions } from '../../services/coffeeMachine';
import { listMerchants } from '../../services/merchant';
import { FULL_PAGE_PARAMS } from '../../services/pagination';
import { listStores } from '../../services/store';

/**
 * 分账三页共用的「引用值 → 名字」字典。
 *
 * # 为什么需要它
 *
 * 分账库里的这几个引用列（`settlement_rules.scope_ref`、`settlement_tasks` 的
 * merchant_ref / brand_ref / store_ref）**装的全是 uuid**：跨域 ID 不进外键（那是分账域的设计，
 * 也是全仓的做法）。但运营不认识 uuid —— 规则列表上摆几列 uuid，「这条规则管的是哪家店」就
 * 永远答不出来。
 *
 * 账户表上原先也有同名三列，**现在没有了**（账户的主体归属由规则表达）。这里保留它们的
 * 取数是因为规则与任务还在用。
 *
 * 后端**没有**帮我们解：分账任务上的三列是**建任务那一刻的快照**（这是对的，历史明细不该
 * 随门店改名而变），而后台要看的恰恰是「现在这个名字」。所以字典取在客户端。
 *
 * # 四个来源各自 try/catch，一个挂了不影响其余三个
 *
 * 这四份列表分属四个权限码（门店 `admin:stores:view`、品牌 `admin:brands:view`、商户
 * `admin:merchants:view`、设备 `coffee_machine:read`）。**一个只管分账的运营完全可能一个都
 * 没有**，那时候这四个请求会回 403 —— 但分账三页本身必须照常打开。所以每一份单独兜住，
 * 取不到就是空字典，页面上退回显示原始 id（`refName` 的行为）。
 *
 * 这也正是 `services/pagination.ts` 里 FULL_PAGE_PARAMS 那条注意事项说的那件事：这些下拉要的
 * 是**全集**，分页之后它们只拿到第 1 页、静默少数据；被拒掉时又不能把整页带崩。
 */

export type RefOption = { label: string; value: string };

export type SettlementRefs = {
  stores: RefOption[];
  brands: RefOption[];
  merchants: RefOption[];
  devices: RefOption[];
  /** id → 名字。四个集合的 id 都是 uuid，不会互相撞，所以合成一张表。 */
  names: Record<string, string>;
};

const EMPTY: SettlementRefs = { stores: [], brands: [], merchants: [], devices: [], names: {} };

/**
 * 取四份引用列表。**永远不抛**：某一份拿不到（没权限、服务抖动）就当它是空的，
 * 其余三份照常返回。
 */
export async function loadSettlementRefs(): Promise<SettlementRefs> {
  const [stores, brands, merchants, devices] = await Promise.all([
    listStores(FULL_PAGE_PARAMS).then(
      (page) =>
        page.items.map((store) => ({
          label: `${store.name}（${store.brandName || store.merchantName || '未知品牌'}）`,
          value: store.id,
          id: store.id,
          name: store.name,
        })),
      () => [],
    ),
    listBrands(FULL_PAGE_PARAMS).then(
      (page) =>
        page.items.map((brand) => ({
          label: `${brand.name}（${brand.merchantName || '未知商户'}）`,
          value: brand.id,
          id: brand.id,
          name: brand.name,
        })),
      () => [],
    ),
    listMerchants(FULL_PAGE_PARAMS).then(
      (page) =>
        page.items.map((merchant) => ({
          label: merchant.name,
          value: merchant.id,
          id: merchant.id,
          name: merchant.name,
        })),
      () => [],
    ),
    listDeviceOptions().then(
      (items) =>
        items.map((device) => ({
          // 设备名 + 出厂序列号：同一家店可能有多台同名设备，序列号是唯一认得出的那个。
          label: `${device.deviceName}（${device.serialUnique}）`,
          value: device.id,
          id: device.id,
          name: device.deviceName,
        })),
      () => [],
    ),
  ]);

  const names: Record<string, string> = {};
  for (const option of [...stores, ...brands, ...merchants, ...devices]) {
    names[option.id] = option.name;
  }

  return {
    stores: stores.map(({ label, value }) => ({ label, value })),
    brands: brands.map(({ label, value }) => ({ label, value })),
    merchants: merchants.map(({ label, value }) => ({ label, value })),
    devices: devices.map(({ label, value }) => ({ label, value })),
    names,
  };
}

export { EMPTY as EMPTY_SETTLEMENT_REFS };

/**
 * 一个引用值该怎么显示：**优先名字，退回原始 id**。
 *
 * 不做「解不出来就显示 —」：解不出来有两种可能——这个引用真的是空的，或者只是字典没取到
 * （没权限/服务抖动）。前者该是「—」，后者**必须把 id 露出来**，否则一条明明配了门店的规则
 * 会显示成没配，而那是这里唯一查得下去的东西。
 */
export function refName(refs: SettlementRefs, value?: string | null): string {
  if (!value) return '—';
  return refs.names[value] ?? value;
}
