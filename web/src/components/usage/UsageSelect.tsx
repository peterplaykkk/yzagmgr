// 用量页的小型选择器（时间范围 / 步长 / 分组维度共用）。
//
// 与原 `UsagePage.tsx` 内联版本**逐字同形**（原生 `select` + 可见 `<label>` 接线；
// UC-047 重构只搬了挂载点，行为与样式零改动）。id 由调用方给，保证同页多控件的 label 各自可靠。

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import { color, fontSize, radius } from '../../theme';

export interface UsageSelectProps {
  id: string;
  label: string;
  value: string;
  options: readonly string[];
  render: (value: string) => string;
  onChange: (next: string) => void;
  disabled?: boolean;
}

export function UsageSelect({
  id,
  label,
  value,
  options,
  render,
  onChange,
  disabled = false,
}: UsageSelectProps) {
  return (
    <Box sx={{ display: 'flex', alignItems: 'center', gap: 0.5 }}>
      <Typography component="label" htmlFor={id} variant="caption" sx={{ color: 'text.secondary' }}>
        {label}
      </Typography>
      <Box
        id={id}
        component="select"
        value={value}
        disabled={disabled}
        onChange={(event) => onChange(event.target.value)}
        sx={{
          // 控件 sm 档（design-system.md §6.2）：高 28 / 水平内距 8 / 圆角 6 / border.input 描边。
          height: 28,
          px: 2,
          border: `1px solid ${color.borderInput}`,
          borderRadius: `${radius.control}px`,
          backgroundColor: color.surface,
          color: color.textPrimary,
          fontSize: fontSize.body,
        }}
      >
        {options.map((option) => (
          <option key={option} value={option}>
            {render(option)}
          </option>
        ))}
      </Box>
    </Box>
  );
}
