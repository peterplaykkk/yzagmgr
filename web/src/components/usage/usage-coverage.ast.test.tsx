import { render, screen, within } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { MemoryRouter } from 'react-router-dom';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { AppProviders } from '../../app/AppProviders';
import i18n from '../../i18n';
import { UsageCharts, type UsageChartsProps } from './charts/UsageCharts';
import { server } from '../../../test/msw/server';
import '../../../test/msw/lifecycle';
import { setMetaToken } from '../../../test/token';

vi.mock('@visactor/react-vchart', async () => {
  const React = await import('react');
  return {
    VChart: (props: { spec?: { type?: string } }) =>
      React.createElement('div', {
        'data-testid': 'usage-coverage-chart',
        'data-chart-type': props.spec?.type ?? '',
      }),
  };
});

function chartsTree(props: UsageChartsProps) {
  return (
    <AppProviders>
      <MemoryRouter initialEntries={['/usage']}>
        <UsageCharts {...props} />
      </MemoryRouter>
    </AppProviders>
  );
}

function renderCharts(overrides: Partial<UsageChartsProps> = {}) {
  setMetaToken('tok_usage_coverage');
  const props: UsageChartsProps = {
    from: '1758800000',
    to: '1758886400',
    bucket: 'hour',
    by: 'all',
    focus: null,
    onFocus: vi.fn(),
    chartsExpanded: false,
    onChartsExpandedChange: vi.fn(),
    failureCodeHref: (status) => `/usage?tab=requests&status=${status}`,
    ...overrides,
  };
  return render(chartsTree(props));
}

beforeEach(() => {
  window.localStorage.clear();
  setMetaToken('tok_usage_coverage');
});

describe('usage AST coverage follow-ups', () => {
  it('renders one honest monitor error for the selected window', async () => {
    server.use(
      http.get('/api/v1/usage/trends', () =>
        HttpResponse.json(
          {
            ok: false,
            error: { code: 'internal', message: 'monitor source unavailable' },
            requestId: 'req_usage_monitor_error',
          },
          { status: 500 },
        ),
      ),
    );
    const monitor = {
      from: '1758800000',
      to: '1758802880',
      bucket: 'minute' as const,
      models: [],
    };

    renderCharts({ monitor });
    const region = await screen.findByTestId('usage-monitor');
    expect(await within(region).findByRole('alert')).toHaveTextContent(
      i18n.t('usage.monitor.errorContext'),
    );
    expect(within(region).queryAllByTestId('usage-coverage-chart')).toHaveLength(0);
    expect(within(region).getAllByRole('button', { name: i18n.t('states.retry') })).toHaveLength(1);
  });
});
