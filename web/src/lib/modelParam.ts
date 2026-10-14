// `model` 参数的解析与归一（UC-110 A1；监控多选与请求明细单值筛选**共用同一参数族**）。
//
// 形态口径：与 Go 侧 `internal/usage.record.go` 的 `modelRe` 逐字同形（模型名封闭字符集、
// ≤200 字符、字符集不含逗号——所以按逗号拆分不会切碎合法名）。形态非法的取值不是「未知
// 模型」：未知但形态合法的选择必须保留（UC-110 边界「未知模型值保留；形态非法值归一」），
// 形态非法的一律在归一中剔除。
//
// 规范形态（URL 唯一写法，供同值比较与 replace 归一）：
// - 逐值 = 去空白后的形态合法名，去重后按**码位升序**（`Array.prototype.sort` 默认序，跨环境确定）；
// - 序列化 = 重复 key `model=a&model=b`（与 `/usage/trends` 的多值参数、请求明细的单值
//   `model=x` 同族；`URLSearchParams.get('model')` 取首个值 ⇒ 明细面天然读到单值）；
// - 读取时兼容逗号分隔写法（`model=a,b`）与重复 key 两种输入，写出一律规范形态；
// - 空集（全非法/未选）⇒ 参数删除（「全部」不写 URL，UC-020 A3 默认值省略）。

/** 与 Go `modelRe` 同形：首字符字母数字，其后 ≤199 个封闭集字符，总长 ≤200。 */
const MODEL_NAME_RE = /^[A-Za-z0-9][A-Za-z0-9._:/@+\[\]()_-]{0,199}$/;

export function isValidModelName(value: string): boolean {
  return MODEL_NAME_RE.test(value);
}

/**
 * 从 URL 读出模型选择（规范形态）：getAll('model') 逐值按逗号拆段 → trim →
 * 丢弃形态非法段 → 去重 → 码位升序。
 */
export function readModelSelection(search: URLSearchParams): string[] {
  const out = new Set<string>();
  for (const raw of search.getAll('model')) {
    for (const segment of raw.split(',')) {
      const value = segment.trim();
      if (value !== '' && isValidModelName(value)) out.add(value);
    }
  }
  return [...out].sort();
}

/** 规范形态的重复 key 序列（空集 = 不写参数）。 */
export function setModelParam(
  search: URLSearchParams,
  selection: readonly string[],
): URLSearchParams {
  const next = new URLSearchParams(search);
  next.delete('model');
  for (const value of [...new Set(selection)].sort()) {
    if (value !== '' && isValidModelName(value)) next.append('model', value);
  }
  return next;
}

/**
 * 当前 URL 是否已是 `selection` 的规范形态（逐值逐位比较）。
 * 非规范输入（逗号写法、乱序、重复、形态非法段）在这里被识别出来，交给 replace 归一。
 */
export function isCanonicalModelParam(
  search: URLSearchParams,
  selection: readonly string[],
): boolean {
  const raw = search.getAll('model');
  return raw.length === selection.length && raw.every((value, index) => value === selection[index]);
}

/**
 * 请求明细（单值筛选面）读取的模型值：取规范序列的首个（多选时明细按第一个过滤，
 * 见 `usage.requests.multiModelNote`）；逗号写法同样取首段。
 * **不**做形态剔除——明细面保持既有「URL 值如实显示」语义（UC-021 A5）。
 */
export function readSingleModel(search: URLSearchParams): string {
  for (const raw of search.getAll('model')) {
    const first = raw.split(',')[0]?.trim() ?? '';
    if (first !== '') return first;
  }
  return '';
}
