# GLM 官方牌价钉价件（glm-pricing.md）

**用途**：GLM 14:00–18:00 高峰 ×3 计费的**基数不变量**——
`pricing` 表里的 GLM 价目行必须逐行等于本文件的官方牌价；UT-6 的「基数核对」用例
解析下方 CSV 与库内价目逐行比对。

- 抓取日期：**2026-10-13**；复核日期：**2026-10-14**（WebFetch 同一官方文档页逐行比对，钉价表
  14/14 行一致——含「不支持缓存命中」模型按 0 落价的口径；`open.bigmodel.cn/pricing` 为 SPA
  空壳不可抓，抓取以 docs 官方文档页为准）
- 来源：<https://docs.bigmodel.cn/cn/guide/start/pricing>（智谱大模型开放平台官方文档 · 价格页）
- 币种：**CNY（元）/ 百万 tokens**；缓存存储费官方标注「限时免费」。
- 官方计费公式（逐字）：`调用费用 = 未命中缓存的输入费用 + 缓存命中费用 + 输出费用 + 缓存存储费用`。
  即：**未命中输入**按 input 档、**命中缓存**按 cache 档、输出按 output 档（写缓存并入 input 档的峰时偏差已登记为已知口径）。
- model 键口径：展示名小写即上游 API 模型 id（`GLM-5.3` → `glm-5.3`）；计价按
  `model_upstream`（回落 `model_requested`）**精确匹配**——若实际上游回名不同（带日期
  后缀等），以实际回名为准录入并同步本表。带输入区间档位的模型见下方「区间档位」段
  （需管理员按实际流量档位择一录入，不进钉价 CSV）。

## 钉价表（机器可读；UT-6 解析本块）

```csv
model,input_per_million,output_per_million,cache_read_per_million,currency
glm-5.3,8,28,2,CNY
glm-5.3-flash,0.8,2.8,0.23,CNY
glm-5.3-flashx,2,7,0.57,CNY
glm-5.2,8,28,2,CNY
glm-4.7-flashx,0.5,3,0.1,CNY
glm-4-plus,5,5,2.5,CNY
glm-4-air-250414,0.5,0.5,0.25,CNY
glm-4-airx,10,10,0,CNY
glm-4-long,1,1,0.5,CNY
glm-z1-air,0.5,0.5,0,CNY
glm-z1-airx,5,5,0,CNY
glm-z1-flashx,0.1,0.1,0,CNY
glm-4-flashx-250414,0.1,0.1,0.05,CNY
glm-ocr,0.2,0.2,0,CNY
```

说明：`cache_read_per_million=0` 表示该模型官方标注「缓存命中不支持」或免费——
0 是**价**（收 0），不是「未知」；未知价的正确表达是模型不在价目表（ledger
`cost_scaled=NULL + price_rule='unpriced'`）。「免费」模型（glm-4.7-flash、
glm-4-flash-250414、glm-z1-flash 等）如需入表按 0/0/0 录入。

## 区间档位（不进钉价 CSV，录入时按实际档位择一）

| 模型（输入区间） | 输入 | 输出 | 缓存命中 |
| --- | --- | --- | --- |
| GLM-5.1（<32K） | 6 | 24 | 1.3 |
| GLM-5.1（≥32K） | 8 | 28 | 2 |
| GLM-5-Turbo（<32K） | 5 | 22 | 1.2 |
| GLM-5-Turbo（≥32K） | 7 | 26 | 1.8 |
| GLM-5（<32K） | 4 | 18 | 1 |
| GLM-5（≥32K） | 6 | 22 | 1.5 |
| GLM-4.7（输入<32K，输出<0.2K） | 2 | 8 | 0.4 |
| GLM-4.7（输入<32K，输出≥0.2K） | 3 | 14 | 0.6 |
| GLM-4.7（输入32K–200K） | 4 | 16 | 0.8 |
| GLM-4.5-Air（输入<32K，输出<0.2K） | 0.8 | 2 | 0.16 |
| GLM-4.5-Air（输入<32K，输出≥0.2K） | 0.8 | 6 | 0.16 |
| GLM-4.5-Air（输入32K–128K） | 1.2 | 8 | 0.24 |
| GLM-5V-Turbo（<32K） | 5 | 22 | 1.2 |
| GLM-5V-Turbo（≥32K） | 7 | 26 | 1.8 |
| GLM-4.6V（<32K） | 1 | 3 | 0.2 |
| GLM-4.6V（32K–128K） | 2 | 6 | 0.4 |

## ×3 计费与本文件的关系

- 高峰窗规则 `glm_peak_x3`（`pricing_rules` 出厂预设）对 `match_provider='glm'` 的
  请求在本地时区 14:00 ≤ t < 18:00（闭开）按 `multiplier_micro=3000000`（×3）乘
  **本表基数**；乘的是「管理端录入价」——录入非官方价时 ×3 会放大偏差，因此
  ① 本文件是唯一录入来源，② UT-6 钉「GLM pricing 行 = 钉价件逐行相等」，
  ③ `PUT /usage/pricing` 对 glm 行偏离本表属已知告警面（软告警）。
- ledger 落的是**写时快照**：改价/改规则不回写历史流水（快照不变量）。

## 已知口径备注（写给账单核对者）

- 本仓 ledger 基础项公式：`B = in×input_price + out×output_price +
  cached×cache_price`，高峰窗 `cost = B×mult`（先除后乘整数域）。
- 该公式与官方公式逐字一致的前提是 **`input_tokens` 列 = 未命中缓存的输入**：
  Anthropic 形态响应天然如此（`input_tokens` 不含 cache 字段）；OpenAI 形态
  `prompt_tokens` **含** cached——嗅探/记账归一时须把 OpenAI 形态的 input 归一为
  `prompt − cached`，否则 cached>0 的请求会比官方多收 `cached×input_price`。
  **已落地**：`internal/proxy/accounting.go`
  `usageFrom` 按 `prompt_tokens_details`/`input_tokens_details` 判 OpenAI 形态并归一
  （`cached>in` 钳 0），Anthropic 形态不扣；守卫用例
  `TestCachedTokensSniffedThreeForms`（原「不扣」断言已按本条款改写并留 record）。
