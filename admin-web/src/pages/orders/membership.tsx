import OrdersTable from './OrdersTable';

/** 会员订单 = 含会员行的订单（后端 hasMembership=true）。 */
export default function MembershipOrdersPage() {
  return <OrdersTable category="membership" />;
}
