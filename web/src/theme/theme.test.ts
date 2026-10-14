import { describe, expect, it } from 'vitest';
import { unstable_styleFunctionSx } from '@mui/system';
import { contrastRatio } from '../../test/contrast';
import { yzagmgrTheme } from './theme';
import { color, spacing, statusTones } from './tokens';

/**
 * 设计 token / 主题的回归护栏（Wave 5 独立审查三条缺陷的「不回退」断言 + R262 W3a 的焦点环取值同步）：
 * 1. 间距阶必须是带 `px` 的字符串——数字会被 MUI 的 `theme.spacing`（= 4）再乘一次，全站间距 ×4；
 * 2. 焦点环：MUI 的 ButtonBase 把 outline 归零，主题必须补回 2px `focus.ring`（品牌蓝 #3370FF；
 *    旧值近黑 #111 已随 R262 设计包 §5/§7.4 替换——可见性断言（2px + 2px offset）不回退）；
 * 3. 对比度红线：次级文字与中性 chip 对各自底色 ≥4.5:1（实算，不靠注释里的数字）；
 * 4. tertiary 解析（W3 核验 issue2）：`text.tertiary` 必须在 palette.text 里可解析，否则
 *    `sx={{ color: 'text.tertiary' }}` 回退原字符串（无效 CSS ⇒ 继承正文色，全库同色）。
 */

type StyleRecord = Record<string, Record<string, unknown> | string | number | undefined>;

function rootOverride(slot: 'MuiButtonBase' | 'MuiOutlinedInput'): Record<string, unknown> {
  const components = yzagmgrTheme.components as Record<string, { styleOverrides?: StyleRecord }>;
  return (components[slot]?.styleOverrides?.root ?? {}) as Record<string, unknown>;
}

describe('设计 token（DESIGN.md §3 / §7）', () => {
  it('间距阶是 px 字符串：数字会被 MUI 再乘 theme.spacing(4)，实得 4 倍', () => {
    const expected: Record<string, number> = {
      x1: 4,
      x2: 8,
      x3: 12,
      x4: 16,
      x6: 24,
      x8: 32,
      x12: 48,
      x16: 64,
    };
    for (const [name, px] of Object.entries(expected)) {
      expect(spacing[name as keyof typeof spacing], name).toBe(`${px}px`);
    }
  });

  it('ButtonBase 家族（Button/IconButton/MenuItem/Tab/...）有键盘焦点环 2px focus.ring + 2px offset', () => {
    const root = rootOverride('MuiButtonBase');
    expect(root['&.Mui-focusVisible']).toEqual({
      outline: `2px solid ${color.focusRing}`,
      outlineOffset: 2,
    });
  });

  it('输入类控件（Outline 面）的聚焦指示是 2px brand.primary 描边（原生 outline 被 MUI 复位，不能只靠它）', () => {
    const root = rootOverride('MuiOutlinedInput');
    expect(root['&.Mui-focused .MuiOutlinedInput-notchedOutline']).toEqual({
      borderWidth: 2,
      borderColor: color.brandPrimary,
    });
  });

  it('对比度红线：次级文字 / 中性 chip 文字对各自底色 ≥4.5:1（Wave 5 实测值回填 DESIGN.md §7）', () => {
    expect(contrastRatio(color.textPrimary, color.canvas)).toBeGreaterThanOrEqual(4.5);
    expect(contrastRatio(color.textSecondary, color.canvas)).toBeGreaterThanOrEqual(4.5);
    expect(contrastRatio(color.textSecondary, color.surface)).toBeGreaterThanOrEqual(4.5);
    expect(contrastRatio(color.textSecondary, color.surfaceSubtle)).toBeGreaterThanOrEqual(4.5);
    expect(contrastRatio(statusTones.neutral.fg, statusTones.neutral.bg)).toBeGreaterThanOrEqual(
      4.5,
    );
  });

  it('text.tertiary（元信息档）在 palette 解析且对比度达标（sx 的 color 通道实测）', () => {
    // 解析事实（W3 核验 issue2 的缺陷根因）：`sx={{ color: 'text.tertiary' }}` 经 @mui/system
    // 的 palette 通道查 `theme.palette.text.tertiary`；palette 缺键时回退为原字符串
    // （无效 CSS ⇒ 继承正文色——全库 67 处此前与普通值同色）。两件事各自钉住：
    // ① palette 里有值；② 经 sx 解析函数实际取出该值（不是只存在于注释里）。
    expect(yzagmgrTheme.palette.text.tertiary).toBe(color.textTertiary);
    const css = unstable_styleFunctionSx({
      theme: yzagmgrTheme,
      sx: { color: 'text.tertiary' },
    }) as Record<string, unknown>;
    expect(css['color']).toBe(color.textTertiary);
    // 对比度（design-system.md §5 实算表）：白卡 4.69 ≥4.5；对 bg.base 4.23 是设计**登记项**
    // （元信息专用，不承载正文/操作），不按正文红线断言（DESIGN.md §7「未达线但暂留」）。
    expect(contrastRatio(color.textTertiary, color.surface)).toBeGreaterThanOrEqual(4.5);
  });
});
