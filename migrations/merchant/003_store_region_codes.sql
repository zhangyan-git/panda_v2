-- merchant/003: stores 增加区划编码列（province_code / city_code / district_code）
--
-- 名字不是稳定的键。数据源一过期就会冒出「下城区」这种早已撤销的区名（NewCoffee
-- 自带的 pca-code.json 里就是这样），而库里只写 district='下城区' 时，将来没有任何
-- 确定性的办法把它迁到新的拱墅区；有 330103 至少能识别、能对照官方变更记录迁移——
-- 编码本身也会废止，但废止是可识别的，自由字符串不能识别、不能校验、不能迁移。
-- 全国区县还有 28 组重名，只有「省 + 市 + 区」三元组能定位；名字打错一个字，这行
-- 数据就永久无法修复。而且编码是事后算不出来的，名字反倒随时能从编码推出来。
--
-- 与名称并列而不是替代名称：列表和详情直接展示名字，回填和迁移用编码。
--
-- 允许为空，且**默认就是空**：本次只做一次尽力而为的回填（cmd/backfill-region），
-- 历史自由文本（比如只写「北京」而不是「北京市」）匹配不上的留空；请求里没带编码
-- 时也保持空。不建字典表、不加外键：本轮唯一的消费者是后台的表单，等小程序改用
-- 同一份主数据时再考虑下发接口。

BEGIN;

ALTER TABLE stores ADD COLUMN IF NOT EXISTS province_code TEXT NOT NULL DEFAULT '';
ALTER TABLE stores ADD COLUMN IF NOT EXISTS city_code     TEXT NOT NULL DEFAULT '';
ALTER TABLE stores ADD COLUMN IF NOT EXISTS district_code TEXT NOT NULL DEFAULT '';

COMMENT ON COLUMN stores.province_code IS '省级区划编码（GB/T 2260），与 province 名称并存；回填不到时为空';
COMMENT ON COLUMN stores.city_code IS '市级区划编码，与 city 名称并存；回填不到时为空';
COMMENT ON COLUMN stores.district_code IS '区县级区划编码，与 district 名称并存；回填不到时为空';

COMMIT;
