-- identity 种子数据：后台控制台的定义——权限码、内置角色、菜单树
--
-- 与 001 分开的理由：这份文件的全部内容是 INSERT，生命周期与建表不同（幂等重跑、
-- ON CONFLICT），混进一千行的建表文件里两边都难读。coupon 集把券类型字典单独放在
-- `002_coupon_seed.sql`，是同一个惯例。
--
-- 单库时代这些内容由 002_menus.sql、003_merchants.sql、005_brands_stores.sql 三家分头
-- 写入，再由 007_admin_permission_view_manage.sql 把动作词表从 read/write 统一成
-- view/manage、由 008_admin_permission_group_labels.sql 把 perm_group 翻成
-- 中文；而 users / roles / permissions / bindings 四组权限码则一直由
-- services/user-service/cmd/seed 写入，从没进过迁移。
--
-- 拆库后这里直接写终态：动作词表就是 view / manage / delete，perm_group 就是中文显示名，
-- 权限码一次写全。于是身份库不再需要「先跑 007/008 收敛」，也不再依赖 cmd/seed 提供权限行
-- ——cmd/seed 仍然负责带凭据的管理员账号，它按 code 查已有的权限行，重跑幂等。
--
-- 同样是终态：后台侧栏只认**已经落地了页面的**菜单，所以历史上被摘掉的几项在这里根本
-- 不出现，而不是「先建后删」——
--   * 「优惠券管理 > 发放优惠券」（/coupons/issue，并入模板列表页）
--   * 「优惠券管理 > 优惠券码」（/coupons/codes，.umirc.ts 里没有这条路由）
--   * 「支付管理 > 支付方式与渠道」（/payments/methods，收钱那四种方式已经是代码里的常量）
--   * 整个「订货管理」子树（订货/库存域已从 V2 整体删除）
-- 对应的权限码也只有最后一项跟着收回（inventory:* 四枚）；上面三项的接口都还在，
-- 所以 coupon:issue 与 coupon:code:manage 一律保留——摘菜单不摘权限。
--
-- 内置角色 super_admin 也在这里建：它是控制台自带角色，不是某台机器的凭据。带密码的管理员
-- 账号仍然由 cmd/seed 创建，迁移不碰凭据。super_admin 在鉴权中间件里直接放行，下面补绑定是
-- 为了前端 access 与菜单回显能拿到码——所以「超管就是全部权限码 + 全部菜单」这句话直接写成
-- 两条 CROSS JOIN，而不是逐个 code 抄一遍。
--
-- 权限码与页面是两回事，菜单与权限码也各自独立：侧边栏看 admin_role_menus、鉴权看
-- admin_role_permissions，两边都绑了才是「看得见且点得动」。
--
-- 幂等：权限码与角色按唯一键判重，菜单按 (父, 名, 路径) 判重，绑定一律 ON CONFLICT DO NOTHING。

BEGIN;

-- ============================================================
-- 权限码
-- ============================================================

INSERT INTO admin_permissions (id, code, perm_group, name, description)
VALUES
  -- 平台自身（用户 / 角色 / 权限 / 绑定 / 菜单 / 商户 / 品牌 / 门店）
  (gen_random_uuid(), 'admin:users:view', '用户管理', '查看管理员', ''),
  (gen_random_uuid(), 'admin:users:manage', '用户管理', '管理管理员', ''),
  (gen_random_uuid(), 'admin:roles:view', '角色管理', '查看角色', ''),
  (gen_random_uuid(), 'admin:roles:manage', '角色管理', '管理角色', ''),
  (gen_random_uuid(), 'admin:roles:delete', '角色管理', '删除角色', ''),
  (gen_random_uuid(), 'admin:permissions:view', '权限管理', '查看权限', ''),
  (gen_random_uuid(), 'admin:permissions:manage', '权限管理', '管理权限', ''),
  (gen_random_uuid(), 'admin:permissions:delete', '权限管理', '删除权限', ''),
  (gen_random_uuid(), 'admin:bindings:view', '绑定管理', '查看绑定', '查看角色权限与用户角色绑定'),
  (gen_random_uuid(), 'admin:bindings:manage', '绑定管理', '管理绑定', '调整角色权限与用户角色绑定'),
  (gen_random_uuid(), 'admin:menus:view', '菜单管理', '查看菜单', '查看后台菜单树'),
  (gen_random_uuid(), 'admin:menus:manage', '菜单管理', '管理菜单', '新建/编辑后台菜单'),
  (gen_random_uuid(), 'admin:menus:delete', '菜单管理', '删除菜单', '删除后台菜单'),
  (gen_random_uuid(), 'admin:merchants:view', '商户管理', '查看商户', '查看商户列表与商户账号'),
  (gen_random_uuid(), 'admin:merchants:manage', '商户管理', '管理商户', '新建/编辑商户、状态流转、维护商户账号'),
  (gen_random_uuid(), 'admin:merchants:delete', '商户管理', '删除商户', '删除商户与商户账号'),
  (gen_random_uuid(), 'admin:brands:view', '品牌管理', '查看品牌', '查看品牌列表'),
  (gen_random_uuid(), 'admin:brands:manage', '品牌管理', '管理品牌', '新建/编辑品牌、启用禁用、审核'),
  (gen_random_uuid(), 'admin:brands:delete', '品牌管理', '删除品牌', '删除品牌'),
  (gen_random_uuid(), 'admin:stores:view', '门店管理', '查看门店', '查看门店列表'),
  (gen_random_uuid(), 'admin:stores:manage', '门店管理', '管理门店', '新建/编辑门店、启用禁用、审核'),
  (gen_random_uuid(), 'admin:stores:delete', '门店管理', '删除门店', '删除门店'),

  -- C 端用户与平台审计
  --
  -- 小程序用户用 admin:miniapp-users:* 而不是 admin:users:*：后者是给平台管理员
  -- （admin_users）用的，两个页面管的是两批完全不同的人。共用同一个码意味着「能看后台
  -- 管理员」的人自动能看全部小程序用户的手机号——这是要把两批人的授权分开的原因，不是
  -- 命名偏好。
  --
  -- 操作日志只有 view 一个码，没有 manage：admin_operation_logs 是审计证据，后台页面只读，
  -- 不提供改和删。加一个 manage 码等于对外承诺有对应的写操作，而那正是这张表不应该有的
  -- 东西——要清理历史得走 DBA 的留存策略，不是管理员在界面上点。
  (gen_random_uuid(), 'admin:miniapp-users:view', '小程序用户', '查看小程序用户',
   '查看小程序用户列表、资料与登录记录'),
  (gen_random_uuid(), 'admin:miniapp-users:manage', '小程序用户', '管理小程序用户',
   '启用/禁用小程序用户；禁用会同时撤销其全部有效会话'),
  (gen_random_uuid(), 'admin:operation-logs:view', '操作日志', '查看操作日志',
   '查看后台操作日志；只读，不包含任何修改或删除能力'),

  -- 优惠券
  --
  -- admin:coupons:* 两枚是 **兼容码**：coupon-service 的接口认的是它们，coupon:* 那一批是
  -- 后台页面用的。两套并存不是没收拾干净，是接口与页面各自的码还没并到一起。
  (gen_random_uuid(), 'coupon:read', '优惠券管理', '查看优惠券', '查看优惠券管理页面与数据'),
  (gen_random_uuid(), 'coupon:type:manage', '优惠券管理', '管理优惠券类型', '创建、编辑和停用优惠券类型'),
  (gen_random_uuid(), 'coupon:template:manage', '优惠券管理', '管理优惠券模板', '创建、编辑和管理优惠券模板'),
  (gen_random_uuid(), 'coupon:template:audit', '优惠券管理', '审核优惠券模板', '审核优惠券模板'),
  (gen_random_uuid(), 'coupon:issue', '优惠券管理', '发放优惠券', '向用户批量发放优惠券'),
  (gen_random_uuid(), 'coupon:issue:override', '优惠券管理', '绕过发券限制', '允许发券时跳过用户领取限制'),
  (gen_random_uuid(), 'coupon:user-coupon:read', '优惠券管理', '查看用户券', '查看用户优惠券及详情'),
  (gen_random_uuid(), 'coupon:user-coupon:redeem', '优惠券管理', '核销用户券', '管理员核销用户优惠券'),
  (gen_random_uuid(), 'coupon:user-coupon:revoke', '优惠券管理', '撤销用户券', '管理员撤销用户优惠券'),
  (gen_random_uuid(), 'coupon:batch:manage', '优惠券管理', '管理发券批次', '查看发券批次和统计'),
  (gen_random_uuid(), 'coupon:code:manage', '优惠券管理', '管理优惠券码', '管理外部券码和实体券批次'),
  (gen_random_uuid(), 'admin:coupons:manage', '优惠券管理', '管理优惠券（兼容）', '兼容现有优惠券服务权限码'),
  (gen_random_uuid(), 'admin:coupons:issue', '优惠券管理', '发放优惠券（兼容）', '兼容现有优惠券服务权限码'),

  -- 设备
  --
  -- 分成写和资金两个码，是因为它们的破坏力差一档：coffee_machine:manage 能改设备资料与
  -- 饮品价格，coffee_machine:balance 能直接动设备的咖啡余额。合成一个码之后，想让运营
  -- 维护饮品目录，就顺手把加钱的能力也给了。
  (gen_random_uuid(), 'coffee_machine:read', '设备管理', '查看设备主数据', '查看厂商、咖啡机设备、饮品与设备供应关系'),
  (gen_random_uuid(), 'coffee_machine:manage', '设备管理', '编辑设备主数据', '新增/修改厂商、咖啡机设备与饮品，以及启用停用与上下架'),
  (gen_random_uuid(), 'coffee_machine:balance', '设备管理', '调整设备余额', '调整咖啡机设备的咖啡余额，操作全程留痕'),

  -- 订单
  --
  -- 三个码按破坏力分档：read 看列表与详情，manage 取消待支付订单（替用户动他的资产，但
  -- 钱还没进来），after-sale:audit 通过或驳回退款申请（同意把钱退回去，比前两个都重，所以
  -- 单独一个码而不是并进 manage——manage 的描述已经写死成「取消待支付订单」，拿来当审核
  -- 退款会让权限描述骗人）。后台的取消与审核都写审计，所以「谁在什么时候做的」事后查得到，
  -- 但权限上仍要先把口子收窄。
  --
  -- 没有 order:*:export 之类的码：导出订单数据目前只有列表分页接口，没有单独的文件导出，
  -- 为一个还不存在的动作发码等于发一个永远勾不动的权限。
  (gen_random_uuid(), 'order:read', '订单管理', '查看订单', '查看订单列表与订单详情，含订单行、支付流水与状态流水'),
  (gen_random_uuid(), 'order:manage', '订单管理', '取消订单', '取消待支付订单，操作会写入平台审计'),
  (gen_random_uuid(), 'order:after-sale:audit', '订单管理', '审核退款申请',
   '通过或驳回用户提交的退款申请，操作会写入平台审计'),

  -- 资产账户（福卡 / 咖啡豆）
  --
  -- 读为什么单独发码、而不是并进 order:read：余额与流水是**别的库**的事实，不随订单接口
  -- 一起给出；而且 order:read 的描述已经写死成「查看订单列表与订单详情」，把福卡流水说成
  -- 订单详情的一部分会让权限描述骗人。
  --
  -- account:manage 是**直接加钱**的能力（adjust 金额带符号，充值就是正数），没有审批环节、
  -- 没有额度上限，也没有第二个人复核。发宽了等于把「凭空造豆」变成一个普通运营动作。真要
  -- 放开，先要做的是给调整加审批流，而不是多绑一个角色。
  (gen_random_uuid(), 'account:read', '资产账户', '查看福卡账户与流水',
   '查看用户的福卡余额与收支流水，含订单赠送与冲正记录'),
  (gen_random_uuid(), 'account:manage', '资产账户', '调整咖啡豆余额',
   '后台人工调整用户的咖啡豆余额（充值为正、纠错为负），带符号金额、全额留痕'),

  -- 抽奖
  --
  -- 三枚码按动作拆，理由不一样：
  --   lottery:read   看开通门店、活动、期次、中奖记录。这一档迟早要发给客服，是查单的延伸。
  --   lottery:manage 开通/停用门店、增删改活动与奖池、作废期次。这一档改的是「用户能抽什么」，
  --                  直接决定中奖名额的分配，破坏力比看列表高一档。
  --   lottery:draw   人工开奖。这是**替系统决定谁中奖**，是本域唯一能凭空造出一份中奖名单的
  --                  动作，与 lottery:manage 分开正是为了让放开前两个码的那天不必连带放开它。
  --
  -- 今天三枚码都只绑 super_admin，所以**真正的收窄不在绑定上**，而在那一路上：人工开奖强制
  -- 填原因、请求体带 expected_round_status + expected_participant_count（拿着过期页面开一局
  -- 已经变了的奖会被 409 挡下）、开奖记录只增不可改（lottery_draws 有触发器）、每次人工开奖
  -- 写一条平台审计。方案 §18.3 要求的「审批」这一环**没做**，补偿控制就是上面这四件事——
  -- 先把话说清楚，免得事后以为这里有审批。
  (gen_random_uuid(), 'lottery:read', '抽奖管理', '查看抽奖',
   '查看开通门店、抽奖活动、期次进度与中奖记录'),
  (gen_random_uuid(), 'lottery:manage', '抽奖管理', '管理抽奖活动',
   '开通或停用门店抽奖、增删改活动与奖池、作废未开奖期次，操作会写入平台审计'),
  (gen_random_uuid(), 'lottery:draw', '抽奖管理', '人工开奖',
   '人工触发一次开奖并生成中奖名单，需填写原因，操作会写入平台审计'),

  -- 会员
  --
  --   membership:read    看套餐、看会员列表与详情、看变更流水。用户问「我是不是会员、
  --                      什么时候到期、为什么被冻了」，答这三句只需要它。
  --   membership:manage  建改套餐：改价、改时长、改会员价路径、上下架。改的是**接下来卖
  --                      什么**，它一个已经卖出去的会员都动不了——memberships 上存着成交
  --                      快照，套餐再改也不会追溯改变已购会员的权益。
  --   membership:adjust  冻结 / 解冻 / 撤销 / 直接改有效期。**这一枚直接白送钱**：把一个人的
  --                      expire_at 往后挪一年，等于送他一年会员价，没有任何订单、支付、流水
  --                      跟着发生。所以刻意不与 manage 合并。
  --
  -- adjust 的每一次调用都写平台审计（方案 §11.6「影响用户资产归属的人工操作」），并在
  -- membership_changes 里留一条带操作人、原因与备注的流水——审计答「谁在什么时候动了手」，
  -- 流水答「会员本身因此变成了什么样」，两句话都要能查到。membership_changes 只增不改不删。
  (gen_random_uuid(), 'membership:read', '会员管理', '查看会员与套餐',
   '查看会员套餐、会员列表与详情、会员变更流水'),
  (gen_random_uuid(), 'membership:manage', '会员管理', '管理会员套餐',
   '新增与修改会员套餐（价格、时长、是否自动续费、会员价路径）、上下架；不影响已购会员'),
  (gen_random_uuid(), 'membership:adjust', '会员管理', '调整会员资格',
   '冻结 / 解冻 / 撤销会员、后台直接调整有效期（需填原因，操作会写入平台审计与会员流水）'),

  -- 支付与分账
  --
  -- payment:read 只读，且**没有配套的 manage**：支付单是**钱的既成事实**，一笔 succeeded
  -- 的单子后面挂着出资行、记账流水、渠道调用、回调通知四张只增表。开任何一个写入口（关单、
  -- 改状态、重放回调）都要先有退款与对账那两件事的语义，而它们没做。收钱那四种方式与渠道
  -- 已经是代码里的常量（payment-service 的 provider/catalog.go），密钥走环境变量，运营没有
  -- 可配的东西，所以也没有「支付方式与渠道」那一页。
  --
  -- 分账的两枚码**绝不并进** payment:*：这是「谁能看支付单」与「谁能改分账配置」的分界。
  -- 前者是所有客服都要的（用户问「我这笔钱扣了没」就得看），后者决定了钱分给谁；并进去之后，
  -- 一次「给他开个支付单查询吧」会连带把分账的写权限一起给出去，而那是这个后台里最不该顺手
  -- 送人的一枚。
  --
  -- 三分里的第三枚（settlement:payout）今天不发：分账在 V2 是**随支付一次下发**的（银联商务
  -- 的 divisionFlag + subOrders 拼在下单报文里，支付成功即分账成功），没有一个「向渠道发起
  -- 打款」的动作可以授权，本地也没有结算单与打款那一层。发一个今天永远勾不出对应行为、勾了
  -- 也没有任何接口认的码，只会让权限页在骗人。真要发的那一天，是「结算单与打款」真做出来的
  -- 那一天。
  (gen_random_uuid(), 'payment:read', '支付管理', '查看支付单',
   '查看支付单列表与详情（出资行、记账流水、状态流转、渠道调用、回调通知）；只读，不包含任何发起、关单或退款能力'),
  (gen_random_uuid(), 'settlement:read', '支付管理', '查看分账配置与明细',
   '查看分账规则、收款账户与分账明细（每条任务分给了谁、分了多少）；只读，不包含任何修改分账配置的能力'),
  (gen_random_uuid(), 'settlement:manage', '支付管理', '管理分账规则与收款账户',
   '新增 / 修改 / 删除分账规则与收款账户，包括每一档分法的比例、固定额与收款方；决定的是「接下来这笔钱怎么分」，已经发生的分账不受影响'),

  -- 开放平台（合作方接入）
  --
  -- 两枚码一起发，是有意的：写接口从第一行代码起就在（合作方与密钥的 CRUD 是这一域的全部
  -- 内容），所以两枚码都指向真实行为。
  --
  -- 签发密钥是 manage，不是 read，且**它的响应是唯一一次出现明文密钥的地方**——这一点在
  -- partner-service 那一侧有测试盯着。码只管「谁能调到这个接口」，挡不住「谁把响应存了下来」，
  -- 所以数据库里另存了掩码，读接口只回掩码。
  (gen_random_uuid(), 'partner:read', '开放平台', '查看合作方与调用日志',
   '查看合作方列表、名下密钥（只显示掩码）、调用日志；只读，不含签发、启停或修改任何访问控制'),
  (gen_random_uuid(), 'partner:manage', '开放平台', '管理合作方与密钥',
   '新增 / 修改合作方，签发新密钥（**明文密钥只在创建响应里出现一次**），启用 / 停用合作方与密钥，修改 IP 白名单与每分钟限流')
ON CONFLICT (code) DO NOTHING;

-- ============================================================
-- 内置超级管理员角色
-- ============================================================

INSERT INTO admin_roles (id, code, name, description, created_at, updated_at)
SELECT gen_random_uuid(), 'super_admin', '超级管理员', '拥有全部权限', NOW(), NOW()
WHERE NOT EXISTS (SELECT 1 FROM admin_roles WHERE code = 'super_admin');

-- 超管在中间件里直接放行，这里补绑定是为了前端 access/菜单回显能拿到权限码。
-- 所以这条规则就是「超管拿到全部权限码」，不必逐个抄 code：权限码加进来的时候它自动就跟上。
INSERT INTO admin_role_permissions (role_id, permission_id)
SELECT r.id, p.id
FROM admin_roles r
CROSS JOIN admin_permissions p
WHERE r.code = 'super_admin'
ON CONFLICT (role_id, permission_id) DO NOTHING;

-- ============================================================
-- 菜单树
-- ============================================================

-- 顶级。目录节点的 path 一律是空串：后台侧边栏来自这张表（menuDataRender 整个替换掉静态
-- 路由里的菜单项），path 不为空的目录会被 ProLayout 当成可跳转的菜单项渲染，点进去是一张
-- 空白页（有子项时不报 404，直接白屏）。
--
-- 概览 / 小程序用户 / 操作日志 三项是叶子而非目录，所以它们有 path：「小程序用户」与
-- 「操作日志」与「优惠券管理」同级，各自只有一个页面，多一层目录只是多一次点击。
--
-- sort 里没有 10：那一格是已删除的「订货管理」。**不要**把 11/12/13 往前挪——排序值一旦
-- 变动，所有人的侧边栏顺序都会在升级那一刻整体挪位，而空着一格没有任何代价。
--
-- 图标名要与 admin-web 的 src/menuIcons.tsx 登记的名字**逐字相同**，否则侧栏渲染不出图标
-- （不报错，只是那一格空着）。后端只存组件名，图标组件由前端映射。
INSERT INTO admin_menus (name, path, icon, sort)
SELECT v.name, v.path, v.icon, v.sort
FROM (VALUES
  ('概览',       '/dashboard',       'DashboardOutlined',    1),
  ('系统管理',   '',                 'SettingOutlined',      2),
  ('商户管理',   '',                 'ShopOutlined',         3),
  ('优惠券管理', '',                 'GiftOutlined',         4),
  ('小程序用户', '/miniapp-users',   'ContactsOutlined',     5),
  ('操作日志',   '/operation-logs',  'HistoryOutlined',      6),
  ('设备管理',   '',                 'CoffeeOutlined',       7),
  ('订单管理',   '',                 'ShoppingCartOutlined', 8),
  ('抽奖管理',   '',                 'TrophyOutlined',       9),
  ('会员管理',   '',                 'CrownOutlined',        11),
  ('支付管理',   '',                 'PayCircleOutlined',    12),
  ('开放平台',   '',                 'ApiOutlined',          13)
) AS v(name, path, icon, sort)
WHERE NOT EXISTS (
  SELECT 1 FROM admin_menus m
  WHERE m.parent_id IS NULL AND m.name = v.name AND m.path = v.path
);

-- 子级：按父名挂载，父行无论刚插入还是已存在都能取到。
--
-- 三处 sort 有缺口，都是历史留下的、同样不要重排：
--   * 优惠券管理下没有 3（发放优惠券）与 6（优惠券码），两项的页面都不存在；
--   * 订单管理的「退款申请」排在 5 而不是 2——列表按**行类型**分成三类，不互斥，
--     「订单列表」保留在最前（客服拿着单号来找单时不知道它属于哪类），分类之后是退款申请。
INSERT INTO admin_menus (parent_id, name, path, icon, sort)
SELECT p.id, v.name, v.path, v.icon, v.sort
FROM (VALUES
  -- 系统管理：装的是 IAM 那一摊。操作日志不放在这里——它覆盖全部业务模块，放进来反而把它说小了。
  ('系统管理', '权限管理',   '/permissions', 'SafetyCertificateOutlined', 1),
  ('系统管理', '角色管理',   '/roles',       'TeamOutlined',              2),
  ('系统管理', '管理员用户', '/admin-users', 'UserOutlined',              3),
  ('系统管理', '菜单管理',   '/menus',       'MenuOutlined',              4),

  ('商户管理', '商户列表',   '/merchants',   'ShopOutlined',              1),
  ('商户管理', '品牌管理',   '/brands',      'TagOutlined',               2),
  ('商户管理', '门店管理',   '/stores',      'EnvironmentOutlined',       3),

  ('优惠券管理', '优惠券类型', '/coupons/types',       'TagsOutlined',           1),
  ('优惠券管理', '优惠券模板', '/coupons/templates',   'FileTextOutlined',       2),
  ('优惠券管理', '发放批次',   '/coupons/batches',     'UnorderedListOutlined',  4),
  ('优惠券管理', '用户优惠券', '/coupons/user-coupons', 'UserOutlined',          5),

  ('设备管理', '咖啡机设备', '/coffee-machines/devices',      'CoffeeOutlined',   1),
  ('设备管理', '厂商管理',   '/coffee-machines/manufacturers', 'ShopOutlined',     2),
  ('设备管理', '饮品管理',   '/coffee-machines/drinks',       'AppstoreOutlined', 3),

  ('订单管理', '订单列表',     '/orders',            'ProfileOutlined',  1),
  ('订单管理', '咖啡订单',     '/orders/coffee',     'CoffeeOutlined',   2),
  ('订单管理', '幸运杯套订单', '/orders/cup-sleeve', 'TagOutlined',      3),
  ('订单管理', '会员订单',     '/orders/membership', 'TeamOutlined',     4),
  ('订单管理', '退款申请',     '/after-sales',       'RollbackOutlined', 5),

  ('抽奖管理', '开通门店', '/lottery/activations', 'EnvironmentOutlined', 1),
  ('抽奖管理', '抽奖活动', '/lottery/campaigns',   'GiftOutlined',        2),
  ('抽奖管理', '期次',     '/lottery/rounds',      'FieldTimeOutlined',   3),
  ('抽奖管理', '中奖记录', '/lottery/wins',        'CrownOutlined',       4),

  -- 会员域只有两个可看的东西——「卖了什么」（套餐）与「谁在会员中」（会员）。
  -- 变更流水不单独挂菜单：它是会员详情页里的时间线，离开那个会员就没有意义。
  -- 包月订阅与店铺码活动是会员身上的附属物，同样挂在这里而不是单开目录——按用户查、按状态筛
  -- 是客服的日常动作，塞进详情页做不到。
  ('会员管理', '会员套餐',   '/membership/plans',         'TagOutlined',       1),
  ('会员管理', '会员列表',   '/membership/members',       'ContactsOutlined',  2),
  ('会员管理', '包月订阅',   '/membership/subscriptions', 'SyncOutlined',      3),
  ('会员管理', '店铺码活动', '/membership/campaigns',     'QrcodeOutlined',    4),

  -- 分账三页留在「支付管理」下而不是单开目录：它们离开那一笔支付就没有意义（规则与账户是
  -- 「这笔钱怎么分」的两半，明细是这笔钱分完的样子）。分账任务的详情不单独挂菜单。
  ('支付管理', '支付单',   '/payments',                   'CreditCardOutlined',    1),
  ('支付管理', '分账规则', '/payments/settlement-rules',  'PartitionOutlined',     2),
  ('支付管理', '分账账户', '/payments/settlement-accounts', 'BankOutlined',        3),
  ('支付管理', '分账明细', '/payments/settlement-tasks',  'ReconciliationOutlined', 4),

  -- 开放平台只有一页。页内三件事：合作方与密钥、调用日志查询。「这家合作方还能不能调」与
  -- 「他手上那几把钥匙是什么状态」摆在一起，因为合作方停用 → 名下**所有**密钥当场失效，
  -- 拆开只会让人来回翻着对。调用日志没有单独发一片叶子：它是同一页上的一个查询区。
  ('开放平台', '合作方',   '/partners', 'SafetyCertificateOutlined', 1)
) AS v(parent, name, path, icon, sort)
JOIN admin_menus p ON p.name = v.parent AND p.parent_id IS NULL
WHERE NOT EXISTS (
  SELECT 1 FROM admin_menus m WHERE m.parent_id = p.id AND m.path = v.path
);

-- 与权限码同一条规则：超管拿到全部菜单。但这里**必须排在插入之后**——侧栏是读这张绑定表的，
-- 先绑后插会漏掉刚建的行。
INSERT INTO admin_role_menus (role_id, menu_id)
SELECT r.id, m.id
FROM admin_roles r
CROSS JOIN admin_menus m
WHERE r.code = 'super_admin'
ON CONFLICT (role_id, menu_id) DO NOTHING;

COMMIT;
