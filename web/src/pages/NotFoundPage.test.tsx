import { MemoryRouter, useLocation } from 'react-router-dom';
import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { AppProviders } from '../app/AppProviders';
import { NotFoundPage } from './NotFoundPage';

function RouteProbe() {
  const location = useLocation();
  return <output data-testid="not-found-location">{location.pathname + location.search}</output>;
}

function renderNotFound(path: string) {
  return render(
    <AppProviders>
      <MemoryRouter initialEntries={[path]}>
        <RouteProbe />
        <NotFoundPage />
      </MemoryRouter>
    </AppProviders>,
  );
}

function setMediaQuery(wide: boolean): () => void {
  const original = Object.getOwnPropertyDescriptor(window, 'matchMedia');
  Object.defineProperty(window, 'matchMedia', {
    configurable: true,
    value: (media: string) => ({
      matches: media === '(min-width:600px)' ? wide : false,
      media,
      onchange: null,
      addListener: () => undefined,
      removeListener: () => undefined,
      addEventListener: () => undefined,
      removeEventListener: () => undefined,
      dispatchEvent: () => false,
    }),
  });
  return () => {
    if (original === undefined) Reflect.deleteProperty(window, 'matchMedia');
    else Object.defineProperty(window, 'matchMedia', original);
  };
}

describe('NotFoundPage · 独立恢复出口', () => {
  it.each([
    { viewport: '窄屏', wide: false },
    { viewport: '桌面', wide: true },
  ])(
    '$viewport exposes real recovery routes without losing the 404 page identity',
    async ({ wide }) => {
      const restore = setMediaQuery(wide);
      try {
        const user = userEvent.setup();
        renderNotFound('/unknown/legacy?from=bookmark');

        expect(screen.getByRole('heading', { name: '未找到' })).toBeInTheDocument();
        expect(screen.getByTestId('not-found-location')).toHaveTextContent(
          '/unknown/legacy?from=bookmark',
        );
        expect(screen.getByRole('link', { name: '返回配置切换' })).toHaveAttribute('href', '/');
        expect(screen.getByRole('link', { name: '查看本机诊断' })).toHaveAttribute(
          'href',
          '/diagnostics',
        );
        expect(screen.getByRole('link', { name: '打开设置' })).toHaveAttribute('href', '/settings');

        await user.click(screen.getByRole('link', { name: '查看本机诊断' }));
        expect(screen.getByTestId('not-found-location')).toHaveTextContent('/diagnostics');
        await user.click(screen.getByRole('link', { name: '打开设置' }));
        expect(screen.getByTestId('not-found-location')).toHaveTextContent('/settings');
        await user.click(screen.getByRole('link', { name: '返回配置切换' }));
        expect(screen.getByTestId('not-found-location')).toHaveTextContent('/');
      } finally {
        restore();
      }
    },
  );
});
