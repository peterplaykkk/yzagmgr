import { describe, expect, it } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { renderRouted } from '../../test/route-probe';
import { setMetaToken } from '../../test/token';
import { state } from '../../test/msw/handlers';
import { server } from '../../test/msw/server';
import '../../test/msw/lifecycle';
import { EMPTY, formatDateTime, formatDurationMs } from '../lib/format';
import zh from '../locales/zh.json';
import { SystemInfoPage } from './SystemInfoPage';

/**
 * `/system`（UC-049）组件级用例：三块（数据占用 / 数据表数据量 / 查询延时）渲染、
 * 字节/行数/延时格式化、`null` 的 `—` 诚实态（含 PG pragma 不适用）、空态、错误可重试、
 * 刷新按钮生效（且不自动轮询）。
 *
 * 数据形状逐字取自 UC-049 冻结契约；默认桩在 `test/msw/handlers.ts`，
 * 本文件用 `server.use` 就地覆写边界面（null / 空 / 错误）。
 */

function renderPage() {
  setMetaToken('tok_test');
  return renderRouted('/system', <SystemInfoPage />);
}

const dictionary = zh as Record<string, string>;
const z = (key: string) => dictionary[key] ?? key;

describe('SystemInfoPage（UC-049）', () => {
  it('三块在场：字节转 GiB/MiB/KiB、行数千分位、延时 ms、pragma 行逐项', async () => {
    const user = userEvent.setup();
    renderPage();
    // ① 数据占用（字节三档格式 + 标题里保留精确字节）。
    expect(await screen.findByTestId('system-storage-dataRootBytes')).toHaveTextContent('3.0 GiB');
    expect(screen.getByTestId('system-storage-usageDbBytes')).toHaveTextContent('18.0 MiB');
    expect(screen.getByTestId('system-storage-dbBytes')).toHaveTextContent('17.9 MiB');
    expect(screen.getByTestId('system-storage-walBytes')).toHaveTextContent('64.0 KiB');
    expect(screen.getByTestId('system-storage-backupsBytes')).toHaveTextContent('240.0 MiB');
    const bytesHelp = screen.getByRole('button', { name: z('system.storage.bytesHelpLabel') });
    await user.click(bytesHelp);
    expect(bytesHelp).toHaveAttribute('aria-expanded', 'true');
    expect(await screen.findByText('18,874,368 B')).toBeInTheDocument();
    expect(screen.getByTestId('system-storage-usageDbBytes').getAttribute('title')).toBe(
      '18,874,368 B',
    );
    // 限额卡（UC-048）内嵌在 ① 里。
    expect(screen.getByText('数据上限与清理')).toBeInTheDocument();
    expect(await screen.findByTestId('limits-db-bytes')).toHaveTextContent('18.0 MiB');

    // ② 数据表数据量（行数千分位）+ request_logs 最旧/最新。
    expect(screen.getByTestId('system-table-rows-request_logs')).toHaveTextContent('1,234,567');
    expect(screen.getByTestId('system-table-rows-meta')).toHaveTextContent('5');
    expect(screen.getByTestId('system-records-oldest')).toHaveTextContent(
      formatDateTime('2026-09-01T00:00:00+08:00'),
    );
    expect(screen.getByTestId('system-records-newest')).toHaveTextContent(
      formatDateTime('2026-09-26T12:00:00+08:00'),
    );

    // ③ 查询延时（op 集逐项；null 显示 —）+ pragma 行。
    expect(screen.getByTestId('system-query-summary-count')).toHaveTextContent('128');
    expect(screen.getByTestId('system-query-summary-avg')).toHaveTextContent(formatDurationMs(3.2));
    expect(screen.getByTestId('system-query-trends-p95')).toHaveTextContent(formatDurationMs(40));
    expect(screen.getByTestId('system-query-pricing-last')).toHaveTextContent(formatDurationMs(1));
    expect(screen.getByTestId('system-query-breakdown-count')).toHaveTextContent(EMPTY);
    expect(screen.getByTestId('system-query-maintenance-avg')).toHaveTextContent(EMPTY);
    const pragma = screen.getByTestId('system-pragma');
    for (const token of [
      'journal_mode=wal',
      'cache_size=-2000',
      'page_size=4096',
      'page_count=4608',
    ]) {
      expect(pragma, token).toHaveTextContent(token);
    }
  });

  it('诚实缺失：records/queryStats/pragma 为 null → `—`；空表给空态文案；notes 原样呈现', async () => {
    server.use(
      http.get('/api/v1/system/info', () =>
        HttpResponse.json({
          ok: true,
          data: {
            storage: {
              dataRootBytes: 1,
              usageDbBytes: 2,
              backupsBytes: 3,
              dbBytes: 4,
              walBytes: 5,
              limits: { maxBytes: 6, applicable: true, usedPct: 7 },
            },
            tables: [],
            records: { oldestAt: null, newestAt: null },
            queryStats: { summary: null },
            pragma: null,
            notes: 'PG 后端：本地文件面不适用。',
          },
          requestId: 'req_test',
        }),
      ),
    );
    renderPage();
    expect(await screen.findByText('当前没有数据表。')).toBeInTheDocument();
    expect(screen.getByTestId('system-records-oldest')).toHaveTextContent(EMPTY);
    expect(screen.getByTestId('system-records-newest')).toHaveTextContent(EMPTY);
    expect(screen.getByTestId('system-query-summary-count')).toHaveTextContent(EMPTY);
    expect(screen.getByTestId('system-query-trends-last')).toHaveTextContent(EMPTY);
    expect(screen.getByTestId('system-pragma-unavailable')).toHaveTextContent(
      '当前后端不适用（PG 后端无 SQLite PRAGMA）。',
    );
    expect(screen.getByTestId('system-notes')).toHaveTextContent('PG 后端：本地文件面不适用。');
  });

  it('空态：没有任何可显示的数据时给空态文案（不渲染三块）', async () => {
    server.use(
      http.get('/api/v1/system/info', () =>
        HttpResponse.json({
          ok: true,
          data: {
            storage: {
              dataRootBytes: 0,
              usageDbBytes: 0,
              backupsBytes: 0,
              dbBytes: 0,
              walBytes: 0,
              limits: { maxBytes: 20 * 1024 * 1024, applicable: true, usedPct: 0 },
            },
            tables: [],
            records: { oldestAt: null, newestAt: null },
            queryStats: null,
            pragma: null,
          },
          requestId: 'req_test',
        }),
      ),
    );
    renderPage();
    expect(await screen.findByText('没有可显示的系统信息。')).toBeInTheDocument();
    expect(screen.queryByTestId('system-storage-dataRootBytes')).toBeNull();
  });

  it('错误可重试：首次 500 → 错误态与重试按钮 → 重试后渲染数据', async () => {
    let calls = 0;
    server.use(
      http.get('/api/v1/system/info', () => {
        calls += 1;
        if (calls === 1) {
          return HttpResponse.json(
            { ok: false, error: { code: 'internal', message: 'stub failure' }, requestId: 'e1' },
            { status: 500 },
          );
        }
        return HttpResponse.json({
          ok: true,
          data: {
            storage: {
              dataRootBytes: 1_073_741_824,
              usageDbBytes: 1_048_576,
              backupsBytes: 0,
              dbBytes: 1_048_576,
              walBytes: 0,
              limits: { maxBytes: 20 * 1024 * 1024, applicable: true, usedPct: 5 },
            },
            tables: [{ name: 'request_logs', rows: 7 }],
            records: { oldestAt: null, newestAt: null },
            queryStats: {},
            pragma: null,
          },
          requestId: 'req_test',
        });
      }),
    );
    renderPage();
    expect(await screen.findByText('系统信息读取失败')).toBeInTheDocument();
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: '重试' }));
    expect(await screen.findByTestId('system-storage-dataRootBytes')).toHaveTextContent('1.0 GiB');
    expect(screen.getByTestId('system-table-rows-request_logs')).toHaveTextContent('7');
  });

  it('「刷新」按钮重取（不自动轮询）：本页与内嵌限额卡同步更新（含维护回显）', async () => {
    const user = userEvent.setup();
    renderPage();
    expect(await screen.findByTestId('system-storage-usageDbBytes')).toHaveTextContent('18.0 MiB');
    expect(await screen.findByTestId('limits-db-bytes')).toHaveTextContent('18.0 MiB');
    expect(screen.getByTestId('limits-last-run')).toHaveTextContent('尚未执行过维护');

    state.usageDbBytes = 2 * 1024 * 1024;
    state.lastMaintenance = {
      lastRunAt: '2026-09-26T13:00:00+08:00',
      deletedRows: 7,
      vacuumed: false,
    };
    await user.click(screen.getByRole('button', { name: '刷新' }));
    await waitFor(() => {
      expect(screen.getByTestId('system-storage-usageDbBytes')).toHaveTextContent('2.0 MiB');
    });
    // 页面「刷新」同时驱动限额卡（refreshKey）：两张「库字节」读数与维护回显同步更新。
    await waitFor(() => {
      expect(screen.getByTestId('limits-db-bytes')).toHaveTextContent('2.0 MiB');
    });
    expect(screen.getByTestId('limits-last-run')).toHaveTextContent(
      formatDateTime('2026-09-26T13:00:00+08:00'),
    );
  });

  it('数字列右对齐 + tabular-nums（表头同步），文本列左对齐（DESIGN.md §1/§4）', async () => {
    renderPage();
    await screen.findByTestId('system-table-rows-request_logs');
    const rowsCell = screen.getByTestId('system-table-rows-request_logs');
    expect(getComputedStyle(rowsCell.closest('td') as HTMLElement).textAlign).toBe('right');
    expect(getComputedStyle(rowsCell).fontVariantNumeric).toBe('tabular-nums');
    const rowsHead = screen.getByText('行数').closest('th') as HTMLElement;
    expect(getComputedStyle(rowsHead).textAlign).toBe('right');
    const nameCell = screen.getByText('request_logs').closest('td') as HTMLElement;
    expect(getComputedStyle(nameCell).textAlign).toBe('left');
    // 查询延时表同口径：次数/平均/P95/最近全部右对齐。
    for (const testid of [
      'system-query-summary-count',
      'system-query-summary-avg',
      'system-query-summary-p95',
      'system-query-summary-last',
    ]) {
      const cell = screen.getByTestId(testid);
      expect(getComputedStyle(cell.closest('td') as HTMLElement).textAlign, testid).toBe('right');
      expect(getComputedStyle(cell).fontVariantNumeric, testid).toBe('tabular-nums');
    }
    expect(getComputedStyle(screen.getByText('次数').closest('th') as HTMLElement).textAlign).toBe(
      'right',
    );
  });

  it('标签列宽统一（共享常量）：页面标签列与内嵌限额卡标签列同宽，值列不错位', async () => {
    renderPage();
    await screen.findByTestId('limits-db-bytes');
    const pageLabel = screen.getByText('数据根目录');
    const cardLabel = screen.getByText('当前库字节');
    expect(getComputedStyle(pageLabel).minWidth).toBe('128px');
    expect(getComputedStyle(cardLabel).minWidth).toBe('128px');
  });

  it('标题层级：三个顶层块是 h2；内嵌限额卡是 h3（不与父块同形）', async () => {
    renderPage();
    await screen.findByTestId('system-storage-dataRootBytes');
    const h2s = screen.getAllByRole('heading', { level: 2 }).map((el) => el.textContent);
    expect(h2s).toEqual(expect.arrayContaining(['数据占用', '数据表数据量', '查询延时']));
    expect(h2s).not.toContain('数据上限与清理');
    expect(screen.getByRole('heading', { level: 3, name: '数据上限与清理' })).toBeInTheDocument();
  });
});
