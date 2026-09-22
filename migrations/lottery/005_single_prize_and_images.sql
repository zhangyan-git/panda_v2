-- 奖池收敛成一个奖品，并补上封面图与海报图。
--
-- 001 建奖池时做的是通用模型：一个活动 1..N 个奖品，每个奖品带 sort_order（开奖时按它
-- 依次分配名额）、prize_kind（coupon/coffee/physical/custom）和 coupon_template_id。
-- 001 的表注释其实已经写着「原型一个活动只有一个 prize 字符串，那落在这一张表的一行上」，
-- 但表格本身留了 N 行的余地，运营侧于是看到了一张可以「加一个奖品」的动态表——而他们从来
-- 只填一个。
--
-- 三列的实情：
--   * sort_order —— 只有一行时它没有意义。「第一档拿满名额才轮到第二档」那套规则没有任何
--     使用场景，还顺带在 service 里长出一条「排序号重复」的校验。
--   * prize_kind —— 从落地起就只存不消费（券服务没有 gRPC，没有兑付动作），列存在期间
--     也没有任何读写方读它做判断，只喂了一个后台 Tag。
--   * coupon_template_id —— 同上，而且只在 prize_kind='coupon' 时有值，等于零行有值。
--
-- 删掉之后一个活动恰好一行奖品，唯一索引从 (campaign_id, sort_order) 换成 (campaign_id)。
-- lottery_wins 上那份 prize_kind 快照一并删：它快照的就是上面这列，概念没了，快照没有意义。
--
-- 两列图片：image_url 改名 cover_image（订单 / 优惠券库里叫的都是这个名字，同一个东西不该
-- 在抽奖库里叫另一个名），并新增 poster_image。**在此之前表单里从来没有 image_url 的输入
-- 框**——它一路从 DDL 贯通到接口，却没人能填，所以存量全是空串，改名不丢数据。
-- 封面必填、海报选填；两张图的比例**待定**，所以注释只写用途不写比例。
--
-- quantity 留着不动：它是「每期开几个人」的封存位，现在恒为 1（表单里没有这个字段），
-- 开期时照样求和冻结成期次的 winner_count。将来要改成一期发多份，从它和
-- service.campaignParams 那一行入手即可。
--
-- 执行前已把 lottery_campaign_prizes / lottery_wins 的现存行整表 dump 到
-- /tmp/panda-run/lottery-single-prize/<stamp>/。DROP COLUMN 与改名不可逆，dev 库没有备份，
-- 这几张表也没有审计，那份 before-image 是唯一的退路。
--
-- 没有 down 段：本仓所有迁移都是单向下发（见 migrations/migrations.go 的包注释）。

-- sort_order 上的 lottery_campaign_prizes_order_unique 会随列一起被 PostgreSQL 删掉，
-- 不需要单独 DROP INDEX（写了反而会撞「index does not exist」，004 刚踩过同一个坑）；
-- prize_kind 的 CHECK 同理随列消失。
ALTER TABLE lottery_campaign_prizes
    DROP COLUMN sort_order,
    DROP COLUMN prize_kind,
    DROP COLUMN coupon_template_id;

ALTER TABLE lottery_campaign_prizes RENAME COLUMN image_url TO cover_image;
ALTER TABLE lottery_campaign_prizes ADD COLUMN poster_image TEXT NOT NULL DEFAULT '';

-- 一个活动恰好一行奖品。这条唯一索引是「只留一个」的**执行者**，不只是约束：
-- repository.replacePrize 靠它把「换奖品」压成一次 UPDATE 或一次 INSERT。
CREATE UNIQUE INDEX lottery_campaign_prizes_campaign_unique
    ON lottery_campaign_prizes (campaign_id);

ALTER TABLE lottery_wins DROP COLUMN prize_kind;

-- 002 里那几条被删列的 COMMENT 不回头改：列没了注释也跟着没了（004 已有先例——002 至今
-- 还写着 lottery_campaigns.end_at，那列早被 004 删了）。这里只重写还活着的列。
COMMENT ON TABLE lottery_campaign_prizes IS '活动的奖池，**一个活动恰好一行**；quantity 是每期的中奖名额（恒为 1），开期时冻结成期次的 winner_count';
COMMENT ON COLUMN lottery_campaign_prizes.campaign_id IS '所属活动，指向本库的 lottery_campaigns；唯一索引确保一个活动只有一行';
COMMENT ON COLUMN lottery_campaign_prizes.cover_image IS '奖品封面图地址，必填；用在活动卡片上。**比例待定**，定了之后要同步改后台表单的提示文案';
COMMENT ON COLUMN lottery_campaign_prizes.poster_image IS '奖品海报图地址，可留空；用在活动详情顶部的横幅。空串时前端回落到自带的那块占位';
COMMENT ON COLUMN lottery_campaign_prizes.quantity IS '每期的中奖名额，当前恒为 1（后台表单里没有这个字段）；开期时冻结到期次的 winner_count，之后改这里不影响已开出的';

COMMENT ON TABLE lottery_wins IS '中奖记录，一次开奖一人一条；奖品名与活动名在这里是快照，之后改奖池不改写历史';
COMMENT ON COLUMN lottery_wins.prize_id IS '奖品 ID，指向本库的 lottery_campaign_prizes；ON DELETE RESTRICT——改奖品走原地 UPDATE，不是删了重插';
