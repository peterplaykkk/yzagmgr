import { describe, expect, it } from 'vitest';
import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useSearchParams } from 'react-router-dom';
import { renderRouted } from '../../test/route-probe';
import { setParams, useGuardedSearchParams } from './url';

/**
 * URL 写入口的同值短路（UC-020 A3「同值重复提交不新增历史」）。
 * 直接测机制：同一个值再写一次不发生导航（`location.key` 不变 = 没压历史条目）。
 */

function Writer() {
  const [search, write] = useGuardedSearchParams();
  return (
    <button type="button" onClick={() => write(setParams(search, { q: 'same' }))}>
      写同值
    </button>
  );
}

/** 对照件：react-router 的裸 setter（不短路同值）——证明断言真的能区分两种行为。 */
function RawWriter() {
  const [search, setSearch] = useSearchParams();
  return (
    <button type="button" onClick={() => setSearch(setParams(search, { q: 'same' }))}>
      裸写同值
    </button>
  );
}

describe('useGuardedSearchParams（UC-020 A3）', () => {
  it('同值写入不导航：location.key 不变（没有新历史条目）', async () => {
    const user = userEvent.setup();
    const { router } = renderRouted('/x?q=same', <Writer />);
    const before = router.state.location.key;
    await user.click(screen.getByRole('button', { name: '写同值' }));
    expect(router.state.location.key).toBe(before);
    expect(router.state.location.search).toBe('?q=same');
  });

  it('不同值写入照常导航：URL 更新且 key 变化', async () => {
    const user = userEvent.setup();
    const { router } = renderRouted('/x', <Writer />);
    const before = router.state.location.key;
    await user.click(screen.getByRole('button', { name: '写同值' }));
    expect(router.state.location.search).toBe('?q=same');
    expect(router.state.location.key).not.toBe(before);
  });

  it('对照：裸 setSearchParams 同值写入**会**导航（根因，断言具备区分力）', async () => {
    const user = userEvent.setup();
    const { router } = renderRouted('/x?q=same', <RawWriter />);
    const before = router.state.location.key;
    await user.click(screen.getByRole('button', { name: '裸写同值' }));
    expect(router.state.location.key).not.toBe(before);
  });
});
