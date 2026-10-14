/**
 * Column width preferences for the model statistics table.
 *
 * The model and provider columns stay compact unless the user explicitly resizes them. Other
 * columns share available width proportionally: they first shrink from their preferred widths to
 * their minimums, then grow without an automatic maximum. Only explicit integer overrides are
 * persisted; flexed values are recalculated when the table viewport changes.
 */

export const MODEL_STATS_WIDTH_STORAGE_KEY = 'yzagmgr.usage.models.columnWidths.v1';
const MODEL_STATS_WIDTH_STORAGE_VERSION = 1;

export const MODEL_STATS_COLUMN_KEYS = [
  'model',
  'provider',
  'requests',
  'inputTokens',
  'outputTokens',
  'tokens',
  'avgTokS',
  'maxTokS',
  'avg',
  'p95',
  'successRate',
  'cost',
] as const;

export type ModelStatsColumnKey = (typeof MODEL_STATS_COLUMN_KEYS)[number];

export interface ModelStatsColumnWidthSpec {
  key: ModelStatsColumnKey;
  defaultWidth: number;
  minWidth: number;
  maxWidth: number;
}

/** One ordered source for the `<colgroup>`, accessible resize bounds, and resolver. */
export const MODEL_STATS_WIDTH_SPECS = [
  { key: 'model', defaultWidth: 160, minWidth: 120, maxWidth: 4096 },
  { key: 'provider', defaultWidth: 72, minWidth: 64, maxWidth: 4096 },
  { key: 'requests', defaultWidth: 76, minWidth: 64, maxWidth: 4096 },
  { key: 'inputTokens', defaultWidth: 100, minWidth: 88, maxWidth: 4096 },
  { key: 'outputTokens', defaultWidth: 100, minWidth: 88, maxWidth: 4096 },
  { key: 'tokens', defaultWidth: 100, minWidth: 88, maxWidth: 4096 },
  { key: 'avgTokS', defaultWidth: 112, minWidth: 96, maxWidth: 4096 },
  { key: 'maxTokS', defaultWidth: 112, minWidth: 96, maxWidth: 4096 },
  { key: 'avg', defaultWidth: 88, minWidth: 80, maxWidth: 4096 },
  { key: 'p95', defaultWidth: 88, minWidth: 80, maxWidth: 4096 },
  { key: 'successRate', defaultWidth: 76, minWidth: 72, maxWidth: 4096 },
  { key: 'cost', defaultWidth: 96, minWidth: 88, maxWidth: 4096 },
] as const satisfies readonly ModelStatsColumnWidthSpec[];

export type ModelStatsWidthOverrides = Partial<Record<ModelStatsColumnKey, number>>;

type StorageAccess = Pick<Storage, 'getItem' | 'setItem' | 'removeItem'>;

function browserStorage(): StorageAccess | undefined {
  try {
    return typeof window === 'undefined' ? undefined : window.localStorage;
  } catch {
    return undefined;
  }
}

function hasOwn(value: object, key: PropertyKey): boolean {
  return Object.prototype.hasOwnProperty.call(value, key);
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function isManualWidth(value: unknown): value is number {
  return typeof value === 'number' && Number.isFinite(value) && Number.isInteger(value);
}

/** Floor proportional results, then give residual pixels to the largest fractional remainders. */
function roundWidthsToTotal(widths: readonly number[], targetTotal: number): number[] {
  const rounded = widths.map((width) => Math.floor(width));
  let residual = targetTotal - rounded.reduce((total, width) => total + width, 0);
  if (residual <= 0) return rounded;

  const order = widths
    .map((width, index) => ({ index, fraction: width - Math.floor(width) }))
    .sort((left, right) => right.fraction - left.fraction || left.index - right.index);
  for (let index = 0; residual > 0; index += 1, residual -= 1) {
    const target = order[index % order.length]!;
    rounded[target.index] = rounded[target.index]! + 1;
  }
  return rounded;
}

/** Clamp a requested explicit width to its column's finite manual bounds. */
export function clampColumnWidth(spec: ModelStatsColumnWidthSpec, width: number): number {
  if (!Number.isFinite(width)) return spec.defaultWidth;
  return Math.min(spec.maxWidth, Math.max(spec.minWidth, Math.round(width)));
}

/**
 * Parse a saved width record. Bad documents/versions fall back as a whole; bad individual values
 * are ignored so the corresponding column keeps its normal responsive default.
 */
export function parseModelStatsWidthOverrides(raw: unknown): ModelStatsWidthOverrides {
  if (typeof raw !== 'string') return {};

  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return {};
  }

  if (!isRecord(parsed) || parsed.version !== MODEL_STATS_WIDTH_STORAGE_VERSION) return {};
  if (!isRecord(parsed.widths)) return {};

  const overrides: ModelStatsWidthOverrides = {};
  for (const spec of MODEL_STATS_WIDTH_SPECS) {
    const value = parsed.widths[spec.key];
    if (hasOwn(parsed.widths, spec.key) && isManualWidth(value)) {
      overrides[spec.key] = clampColumnWidth(spec, value);
    }
  }
  return overrides;
}

/** Read preferences without allowing unavailable or blocked storage to affect the table. */
export function readModelStatsWidthOverrides(
  storage: StorageAccess | undefined = browserStorage(),
): ModelStatsWidthOverrides {
  if (storage === undefined) return {};
  try {
    return parseModelStatsWidthOverrides(storage.getItem(MODEL_STATS_WIDTH_STORAGE_KEY));
  } catch {
    return {};
  }
}

/** Persist only known explicit integer widths; return false when storage is unavailable. */
export function writeModelStatsWidthOverrides(
  overrides: ModelStatsWidthOverrides,
  storage: StorageAccess | undefined = browserStorage(),
): boolean {
  if (storage === undefined) return false;

  const widths: ModelStatsWidthOverrides = {};
  for (const spec of MODEL_STATS_WIDTH_SPECS) {
    const value = overrides[spec.key];
    if (hasOwn(overrides, spec.key) && isManualWidth(value)) {
      widths[spec.key] = clampColumnWidth(spec, value);
    }
  }

  try {
    storage.setItem(
      MODEL_STATS_WIDTH_STORAGE_KEY,
      JSON.stringify({ version: MODEL_STATS_WIDTH_STORAGE_VERSION, widths }),
    );
    return true;
  } catch {
    return false;
  }
}

/** Clear saved overrides for the reset action; report failure without throwing. */
export function clearModelStatsWidthOverrides(
  storage: StorageAccess | undefined = browserStorage(),
): boolean {
  if (storage === undefined) return false;
  try {
    storage.removeItem(MODEL_STATS_WIDTH_STORAGE_KEY);
    return true;
  } catch {
    return false;
  }
}

/**
 * Resolve the ordered, rendered pixel widths for the current table viewport.
 *
 * Manual columns and the default model/provider columns are fixed. Remaining flexible numeric
 * columns shrink toward their per-column minimums in proportion to each column's shrink capacity,
 * then grow in proportion to their preferred widths. All automatic widths stay finite, and their
 * total reaches the available width whenever flexible columns can absorb it.
 */
export function resolveModelStatsColumnWidths(
  availableWidth: number,
  overrides: ModelStatsWidthOverrides,
): number[] {
  const validOverrides: ModelStatsWidthOverrides = {};
  for (const spec of MODEL_STATS_WIDTH_SPECS) {
    const value = overrides[spec.key];
    if (hasOwn(overrides, spec.key) && isManualWidth(value)) {
      validOverrides[spec.key] = clampColumnWidth(spec, value);
    }
  }

  const widths = MODEL_STATS_WIDTH_SPECS.map((spec) =>
    hasOwn(validOverrides, spec.key) ? validOverrides[spec.key]! : spec.defaultWidth,
  );
  const flexibleIndexes = MODEL_STATS_WIDTH_SPECS.flatMap((spec, index) =>
    spec.key !== 'model' && spec.key !== 'provider' && !hasOwn(validOverrides, spec.key)
      ? [index]
      : [],
  );
  if (flexibleIndexes.length === 0) return widths;

  const fixedWidth = widths.reduce(
    (total, width, index) => total + (flexibleIndexes.includes(index) ? 0 : width),
    0,
  );
  const flexibleDefaultWidth = flexibleIndexes.reduce(
    (total, index) => total + MODEL_STATS_WIDTH_SPECS[index]!.defaultWidth,
    0,
  );
  const flexibleMinWidth = flexibleIndexes.reduce(
    (total, index) => total + MODEL_STATS_WIDTH_SPECS[index]!.minWidth,
    0,
  );
  const preferredTotal = fixedWidth + flexibleDefaultWidth;
  const safeAvailableWidth =
    Number.isFinite(availableWidth) && availableWidth > 0 && Number.isSafeInteger(availableWidth)
      ? availableWidth
      : preferredTotal;
  const targetFlexibleWidth = safeAvailableWidth - fixedWidth;

  if (targetFlexibleWidth <= flexibleMinWidth) {
    for (const index of flexibleIndexes) {
      widths[index] = MODEL_STATS_WIDTH_SPECS[index]!.minWidth;
    }
    return widths;
  }

  if (targetFlexibleWidth < flexibleDefaultWidth) {
    const totalShrinkCapacity = flexibleDefaultWidth - flexibleMinWidth;
    const shrinkRatio = (flexibleDefaultWidth - targetFlexibleWidth) / totalShrinkCapacity;
    const proportionalWidths = flexibleIndexes.map((index) => {
      const spec = MODEL_STATS_WIDTH_SPECS[index]!;
      return spec.defaultWidth - (spec.defaultWidth - spec.minWidth) * shrinkRatio;
    });
    const resolvedWidths = roundWidthsToTotal(proportionalWidths, targetFlexibleWidth);
    for (const [offset, index] of flexibleIndexes.entries()) {
      widths[index] = resolvedWidths[offset]!;
    }
    return widths;
  }

  const extraWidth = targetFlexibleWidth - flexibleDefaultWidth;
  const proportionalWidths = flexibleIndexes.map((index) => {
    const spec = MODEL_STATS_WIDTH_SPECS[index]!;
    return spec.defaultWidth + (extraWidth * spec.defaultWidth) / flexibleDefaultWidth;
  });
  const resolvedWidths = roundWidthsToTotal(proportionalWidths, targetFlexibleWidth);
  for (const [offset, index] of flexibleIndexes.entries()) {
    widths[index] = resolvedWidths[offset]!;
  }
  return widths;
}
