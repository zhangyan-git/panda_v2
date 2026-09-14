import OrdersTable from './OrdersTable';

/** 咖啡订单 = 含饮品行的订单（后端 hasDrink=true）。 */
export default function CoffeeOrdersPage() {
  return <OrdersTable category="drink" />;
}
