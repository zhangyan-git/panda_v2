import OrdersTable from './OrdersTable';

/**
 * 幸运杯套订单 = 含加购行的订单（后端 hasAddon=true）。
 *
 * 「杯套」是原型里的叫法，库里存的是 line_type='addon'（加购活动商品）。两者是一回事，
 * 页面文案跟菜单走，字段名跟库里走。
 */
export default function CupSleeveOrdersPage() {
  return <OrdersTable category="addon" />;
}
