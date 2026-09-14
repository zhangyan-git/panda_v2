-- ============================================================
-- 饮品并回设备：drinks 一行即「某台设备上的一杯饮品」
-- ============================================================

-- 老系统 drinks 集合就是每台设备一行饮品（后台那个表单里 device_id 与 manufacturer_id
-- 并存，两者不是二选一）。V2 起初把它拆成「厂商级目录 drinks + 供应关系 device_drinks」，
-- 拆完之后没有任何入口能把目录行挂到设备上——device_drinks 一直是 0 行，设备详情那一屏
-- 永远是空的。这里按老系统的形状并回来：设备直接存在饮品行上，价格就是这台设备上的售价，
-- 不再有「每机覆盖价 / 目录价」两套说法。

-- 可空，因为库里可能已经有还没挂设备的行（本仓 dev 库就有两行）。挂设备这件事由
-- 表单和接口强制，等库里没有空行之后再单独一版收紧成 NOT NULL。
ALTER TABLE drinks ADD COLUMN device_id UUID REFERENCES devices(id) ON DELETE CASCADE;
COMMENT ON COLUMN drinks.device_id IS '设备 ID，引用本库 devices；为空表示这行还没挂到设备上';

-- device_drinks 的 enabled 与 sort_order 不另开列：本表已有 status（on_shelf/off_shelf）
-- 与 sort，语义相同，直接用。

-- 判重键带上设备：同一款饮品在 N 台设备上就是 N 行，这是这个模型的应有之义。厂商侧同步
-- 将来按 (设备, 厂商, 厂商侧 ID) 判重，而不是原来那个全局唯一的厂商侧 ID。
DROP INDEX drinks_manufacturer_origin_unique;
CREATE UNIQUE INDEX drinks_device_origin_unique
    ON drinks (device_id, manufacturer_id, origin_id)
    WHERE origin_id <> '';

-- 设备详情那一屏就是按设备取饮品，排序列与 devices_store_idx 同形。
CREATE INDEX drinks_device_idx ON drinks (device_id, status);

-- 供应关系表拆掉：它已经空了，且每一列的语义都已并入 drinks，留着就是两套真相。
DROP TABLE device_drinks;
