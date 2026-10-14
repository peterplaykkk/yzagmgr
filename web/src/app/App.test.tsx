import { render, screen, waitFor } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { App } from './App';
import { setMetaToken } from '../../test/token';
import '../../test/msw/lifecycle';

describe('App（浏览器入口装配）', () => {
  it('从深链启动时保留 browser history 路由并渲染目标页', async () => {
    window.history.replaceState(null, '', '/arms');
    setMetaToken('tok_test');
    render(<App />);

    await waitFor(() => expect(window.location.pathname).toBe('/arms'));
    expect(await screen.findByRole('heading', { level: 1 })).toHaveTextContent('请求映射');
    expect(document.title).toBe('请求映射 | yzagmgr');
  });
});
