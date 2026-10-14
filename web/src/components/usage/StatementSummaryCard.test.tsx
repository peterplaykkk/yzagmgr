import { describe, expect, it } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { renderWithProviders } from '../../../test/render';
import { setMetaToken } from '../../../test/token';
import { server } from '../../../test/msw/server';
import '../../../test/msw/lifecycle';
import { StatementSummaryCard } from './StatementSummaryCard';

/**
 * 账单汇总卡（v8 §1.4③）：未选用户=提示不报错；选定用户=币种/模型/规则分桶 +
 * 估算对照 + 无价条数（不报 0）；请求形状带窗口。
 */

function renderCard(slug: string | null) {
  setMetaToken('tok_test');
  return renderWithProviders(
    <StatementSummaryCard slug={slug} from="2026-10-13T08:00:00+08:00" to="2026-10-13T10:00:00+08:00" />,
  );
}

const ok = (data: unknown) =>
  HttpResponse.json({ ok: true, data, requestId: 'req_test' }, { status: 200 });

describe('StatementSummaryCard（账单汇总）', () => {
  it('未选用户：显示选择提示，不发请求也不报错', () => {
    renderCard(null);
    expect(screen.getByText('选择用户后显示其账单汇总（按币种分桶，不换汇）。')).toBeInTheDocument();
  });

  it('选定用户：请求带 slug 与窗口，分桶与无价条数如实呈现', async () => {
    const urls: string[] = [];
    server.use(
      http.get('/api/v1/usage/statement/:slug', ({ request }) => {
        urls.push(String(request.url));
        return ok({
          slug: 'alice',
          byCurrency: [
            { key: 'CNY', requests: 1, inputTokens: 1200, outputTokens: 400, cachedTokens: 100, cost: 0.0231 },
          ],
          byModel: [
            { key: 'glm-5-flash', requests: 1, inputTokens: 1200, outputTokens: 400, cachedTokens: 100, cost: 0.0231 },
          ],
          byRule: [
            { key: 'glm_peak_x3', requests: 1, inputTokens: 1200, outputTokens: 400, cachedTokens: 100, cost: 0.0231 },
          ],
          estimated: [
            { key: 'CNY', requests: 1, inputTokens: 1200, outputTokens: 400, cachedTokens: 100, cost: 0.0077 },
          ],
          asOf: '2026-10-13T10:05:00+08:00',
          unpricedRequests: 1,
        });
      }),
    );
    renderCard('alice');
    expect(await screen.findByRole('heading', { name: 'alice 的账单' })).toBeInTheDocument();
    await waitFor(() => expect(urls.length).toBeGreaterThan(0));
    expect(urls[0]).toContain('/api/v1/usage/statement/alice');
    expect(urls[0]).toContain('from=2026-10-13T08');
    expect(screen.getByText('按币种')).toBeInTheDocument();
    expect(screen.getByText('按模型')).toBeInTheDocument();
    expect(screen.getByText('按规则')).toBeInTheDocument();
    expect(screen.getByText('基础价估算（对照）')).toBeInTheDocument();
    expect(screen.getByText('1 条无价记录，不计入金额。')).toBeInTheDocument();
    expect(screen.getByText('glm_peak_x3')).toBeInTheDocument();
    expect(screen.getByText('¥0.02')).toBeInTheDocument();
  });
});
