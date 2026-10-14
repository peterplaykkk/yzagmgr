// UC-096 解释预览区块（/arms 请求映射页）：输入模型（+ 可选 effort）→ 匹配链 →
// 预计去向 → effort 请求值/实际值/原因 → 来源归因 → 规则冲突 → 生效规则版本（A6）。
//
// 端点是**纯计算**的（`POST /routes/explain`：不联网、不读 live、不触盘、不建租约，
// 见 `doc/api-v1.md` §7.1「解释预览」段）：本区块因此可以安全地反复试输入——
// 不做任何写请求、不缓存、不落任何状态。
//
// 诚实性纪律（与冻结契约同源）：
// - 预计结果与「实际结果」分开表述：本区块只给**预计**（匹配链/去向/effort 解析），
//   真实请求核对入口在请求明细与运行时证据面（A6）；
// - outcome/effort reason 从**机器码**渲染文案（`lib/routeExplain`），未知码原样透出；
// - 空表（未配置）⇒ 明确透传说明，不画虚假映射；
// - 解释失败/结果不可用如实呈现；环境变量对生效面的覆盖不在解释范围内——
//   只读提示 + 既有「设置 → 环境变量冲突」入口（不越界实现诊断面）。

import { useEffect, useRef, useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Link as RouterLink } from 'react-router-dom';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import MuiLink from '@mui/material/Link';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import { explainRoute } from '../api/endpoints';
import { toAgmgrApiError, type AgmgrApiError } from '../api/errors';
import { EFFORT_WHITELIST, type RouteExplainResult } from '../api/types';
import { Callout } from './Callout';
import { ContextHelp } from './ContextHelp';
import { Mono, MonoValue } from './MonoValue';
import { RouteConflictPreview } from './RouteConflictPreview';
import { Section } from './Section';
import { ErrorState } from './States';
import { StatusChip } from './StatusChip';
import { EMPTY } from '../lib/format';
import {
  effortReasonText,
  outboundEffortText,
  outcomeText,
  refusalReasonText,
  requestedEffortText,
  ruleVersionText,
  versionSourceText,
} from '../lib/routeExplain';
import { color, fontFamily, fontSize, radius, spacing } from '../theme';

const MODEL_FIELD_ID = 'route-explain-model';
const EFFORT_FIELD_ID = 'route-explain-effort';

/** outcome → 徽标语义色（matched=info 不夸大；refused=danger；passthrough=neutral）。 */
function outcomeTone(outcome: string): 'info' | 'neutral' | 'danger' {
  if (outcome === 'matched') return 'info';
  if (outcome === 'refused') return 'danger';
  return 'neutral';
}

/** 一行「标签 + 值」（与运行时证据面的 RuntimeField 同形；值为空串如实给 `—`）。 */
function ExplainField({ label, children }: { label: string; children: ReactNode }) {
  return (
    <Box sx={{ display: 'flex', alignItems: 'baseline', gap: 1, minWidth: 0, flexWrap: 'wrap' }}>
      <Typography
        component="span"
        variant="caption"
        sx={{ color: 'text.secondary', minWidth: 96, flexShrink: 0 }}
      >
        {label}
      </Typography>
      <Box component="span" sx={{ minWidth: 0 }}>
        {children}
      </Box>
    </Box>
  );
}

/** 结果块：outcome + 匹配链 + 去向 + effort 解析 + 归因 + 冲突 + 规则版本。 */
function ExplainResultView({ result }: { result: RouteExplainResult }) {
  const { t } = useTranslation();
  const chainHits: string[] = [];
  if (result.matchedBy === 'arm') {
    chainHits.push(t('router.explain.chain.arm', { arm: result.arm ?? '' }));
  } else if (result.matchedBy === 'alias') {
    chainHits.push(
      t('router.explain.chain.alias', {
        alias: result.alias ?? '',
        arm: result.arm ?? '',
        target: result.outboundModel,
      }),
    );
  } else {
    chainHits.push(t('router.explain.chain.noMatch'));
  }

  return (
    <Box
      aria-live="polite"
      sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x2, minWidth: 0 }}
    >
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, flexWrap: 'wrap' }}>
        <StatusChip tone={outcomeTone(result.outcome)} label={outcomeText(result.outcome, t)} />
        <Typography component="p" variant="caption" sx={{ color: 'text.secondary', minWidth: 0 }}>
          {result.basis}
        </Typography>
      </Box>

      {/* 匹配链：请求模型 → 命中的规则 → 预计出站模型（解析卡的中段；mono 值 + `→` 连接符，
          文案逐字沿用）。块底/圆角为视觉分组，不改任何字段结构与可访问名。 */}
      <Box
        sx={{
          display: 'flex',
          flexDirection: 'column',
          gap: spacing.x1,
          minWidth: 0,
          backgroundColor: color.surfaceSubtle,
          borderRadius: `${radius.md}px`,
          px: spacing.x3,
          py: spacing.x2,
        }}
      >
        <Typography component="p" variant="overline" sx={{ display: 'block', m: 0 }}>
          {t('router.explain.chain.title')}
        </Typography>
        <ExplainField label={t('router.explain.chain.request')}>
          <Mono>{result.model}</Mono>
        </ExplainField>
        {chainHits.map((line) => (
          <ExplainField key={line} label={t('router.explain.chain.match')}>
            <Typography
              component="span"
              sx={{
                fontFamily: fontFamily.mono,
                fontSize: fontSize.mono,
                overflowWrap: 'anywhere',
              }}
            >
              {line}
            </Typography>
          </ExplainField>
        ))}
        <ExplainField label={t('router.explain.chain.outbound')}>
          <Mono>{result.outboundModel === '' ? EMPTY : result.outboundModel}</Mono>
        </ExplainField>
        <ExplainField label={t('router.explain.chain.upstream')}>
          <Typography component="span" variant="caption">
            {result.upstreamContacted
              ? t('router.explain.chain.upstream.yes')
              : t('router.explain.chain.upstream.no')}
          </Typography>
        </ExplainField>
      </Box>

      {/* 预计去向：端点类别（不含密钥材料）。 */}
      <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5, minWidth: 0 }}>
        <Typography component="p" variant="overline" sx={{ display: 'block', m: 0 }}>
          {t('router.explain.destination.title')}
        </Typography>
        <ExplainField label={t('router.explain.destination.kind')}>
          <Typography component="span" variant="caption">
            {t(`router.explain.destination.kind.${result.destination.kind}`, {
              defaultValue: result.destination.kind,
            })}
          </Typography>
        </ExplainField>
        {result.destination.endpointKind === undefined ? null : (
          <ExplainField label={t('router.explain.destination.endpointKind')}>
            <Typography component="span" variant="caption">
              {t(`router.explain.destination.endpointKind.${result.destination.endpointKind}`, {
                defaultValue: result.destination.endpointKind,
              })}
            </Typography>
          </ExplainField>
        )}
        {result.destination.endpoint === undefined || result.destination.endpoint === '' ? null : (
          <ExplainField label={t('router.explain.destination.endpoint')}>
            <MonoValue
              value={result.destination.endpoint}
              copyLabel={t('router.copyEndpoint', { value: result.destination.endpoint })}
              wrap
            />
          </ExplainField>
        )}
        <Typography component="p" variant="caption" sx={{ color: 'text.secondary' }}>
          {result.destination.detail}
        </Typography>
        {result.outcome === 'passthrough' ? (
          // 空表透传：与 tool.action.router.emptyPassthrough 同口径（未配置 ⇒ 直连网关、
          // 与 yzagmgr 配置等价、不为默认值要求重复规则），按本页语境措辞。
          // W5d 精简：短句上屏；完整口径转 title 提示。
          <Callout tone="info" title={t('router.explain.destination.passthroughNoteDetail')}>
            {t('router.explain.destination.passthroughNote')}
          </Callout>
        ) : null}
      </Box>

      {/* effort 解析：请求值 → 实际值 → 原因（A3）。 */}
      <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5, minWidth: 0 }}>
        <Typography component="p" variant="overline" sx={{ display: 'block', m: 0 }}>
          {t('router.explain.effort.title')}
        </Typography>
        <ExplainField label={t('router.explain.effort.requested')}>
          <Typography component="span" variant="caption">
            {requestedEffortText(result.effort.requested, t)}
          </Typography>
        </ExplainField>
        <ExplainField label={t('router.explain.effort.outbound')}>
          <Typography component="span" variant="caption">
            {outboundEffortText(result.effort.outbound, t, result.effort.outcome)}
          </Typography>
        </ExplainField>
        <ExplainField label={t('router.explain.effort.reason')}>
          <Typography component="span" variant="caption">
            {effortReasonText(result.effort.reason, t)}
          </Typography>
        </ExplainField>
        <Typography component="p" variant="caption" sx={{ color: 'text.secondary' }}>
          {result.effort.detail}
        </Typography>
      </Box>

      {/* 来源归因（A2：arm vs alias 的匹配依据；不串供应商）。 */}
      <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5, minWidth: 0 }}>
        <Typography component="p" variant="overline" sx={{ display: 'block', m: 0 }}>
          {t('router.explain.attribution.title')}
        </Typography>
        <ExplainField label={t('router.explain.attribution.matchedBy')}>
          <Typography component="span" variant="caption">
            {t(`router.explain.attribution.source.${result.matchedBy}`, {
              defaultValue: result.matchedBy,
            })}
          </Typography>
        </ExplainField>
      </Box>

      {/* 规则冲突（A5）：生效规则集里的现存冲突（应用前预览面在编辑对话框里）。 */}
      <RouteConflictPreview conflicts={result.conflicts} context="live" />

      {/* 生效规则版本（A6）：与配置版本 appliedTx 区分；空串 = 未知。 */}
      <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5, minWidth: 0 }}>
        <ExplainField label={t('ruleVersion.label')}>
          <Typography
            component="span"
            sx={{ fontFamily: fontFamily.mono, fontSize: fontSize.mono, overflowWrap: 'anywhere' }}
          >
            {ruleVersionText(result.ruleVersion, t)}
          </Typography>
        </ExplainField>
        <Typography component="p" variant="caption" sx={{ color: 'text.tertiary' }}>
          {t('router.explain.version.note', {
            source: versionSourceText(result.ruleVersionSource, t),
          })}
        </Typography>
      </Box>
    </Box>
  );
}

export function RouteExplainPanel() {
  const { t } = useTranslation();
  const [model, setModel] = useState('');
  const [effort, setEffort] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<AgmgrApiError | null>(null);
  const [result, setResult] = useState<RouteExplainResult | null>(null);
  const requestSequence = useRef(0);
  const requestController = useRef<AbortController | null>(null);

  const clearAssessment = () => {
    requestSequence.current += 1;
    requestController.current?.abort();
    requestController.current = null;
    setBusy(false);
    setError(null);
    setResult(null);
  };

  useEffect(
    () => () => {
      requestSequence.current += 1;
      requestController.current?.abort();
    },
    [],
  );

  const run = () => {
    const requestModel = model.trim();
    if (requestModel === '') return;
    const sequence = requestSequence.current + 1;
    requestSequence.current = sequence;
    requestController.current?.abort();
    const controller = new AbortController();
    requestController.current = controller;
    setBusy(true);
    setError(null);
    void explainRoute(
      {
        model: requestModel,
        // 缺席 = 请求体不带 effort 字段（与显式档位语义不同）；空选项即缺席。
        ...(effort === '' ? {} : { effort }),
      },
      { signal: controller.signal },
    ).then(
      (payload) => {
        if (requestSequence.current !== sequence) return;
        requestController.current = null;
        setBusy(false);
        setResult(payload);
      },
      (err: unknown) => {
        if (requestSequence.current !== sequence) return;
        requestController.current = null;
        setBusy(false);
        setResult(null);
        setError(toAgmgrApiError(err));
      },
    );
  };

  return (
    <Section
      title={t('router.explain.title')}
      id="route-explain"
      actions={
        <ContextHelp
          label={t('router.explain.helpLabel')}
          heading={t('router.explain.helpHeading')}
        >
          {t('router.explain.introDetail')}
        </ContextHelp>
      }
    >
      <Box sx={{ display: 'flex', alignItems: 'flex-end', gap: spacing.x2, flexWrap: 'wrap' }}>
        <TextField
          id={MODEL_FIELD_ID}
          size="small"
          label={t('router.explain.model')}
          value={model}
          sx={{ minWidth: 240, '& .MuiInputBase-root': { minHeight: 44 } }}
          onChange={(event) => {
            setModel(event.target.value);
            clearAssessment();
          }}
          onKeyDown={(event) => {
            if (event.key === 'Enter' && model.trim() !== '' && !busy) run();
          }}
        />
        <ContextHelp
          label={t('router.explain.modelHintLabel')}
          heading={t('router.explain.modelHintHeading')}
        >
          {t('router.explain.modelHint')}
        </ContextHelp>
        <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
          <Typography component="label" htmlFor={EFFORT_FIELD_ID} variant="caption">
            {t('router.explain.effortLabel')}
          </Typography>
          <Box
            id={EFFORT_FIELD_ID}
            component="select"
            aria-label={t('router.explain.effortLabel')}
            value={effort}
            onChange={(event) => {
              setEffort(event.target.value);
              clearAssessment();
            }}
            sx={{
              height: 44,
              px: 1,
              border: `1px solid ${color.borderInput}`,
              borderRadius: `${radius.control}px`,
              backgroundColor: color.surface,
              fontSize: fontSize.body,
            }}
          >
            <option value="">{t('router.explain.effortAbsentOption')}</option>
            {EFFORT_WHITELIST.map((value) => (
              <option key={value} value={value}>
                {value}
              </option>
            ))}
          </Box>
        </Box>
        <Button
          type="button"
          variant="contained"
          disabled={busy || model.trim() === ''}
          onClick={run}
          sx={{ minHeight: 44 }}
        >
          {busy ? t('router.explain.running') : t('router.explain.run')}
        </Button>
      </Box>

      {error === null ? null : (
        <ErrorState error={error} context={t('router.explain.errorContext')} retry={run} compact />
      )}

      {result === null ? (
        error === null ? null : null
      ) : (
        <Box sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x2, minWidth: 0 }}>
          <Box
            aria-live="polite"
            sx={{ display: 'flex', alignItems: 'center', gap: 1, flexWrap: 'wrap', minWidth: 0 }}
          >
            <StatusChip tone={outcomeTone(result.outcome)} label={outcomeText(result.outcome, t)} />
            <Mono>{result.model}</Mono>
            <Typography component="span" aria-hidden="true" sx={{ color: 'text.tertiary' }}>
              →
            </Typography>
            <Mono>{result.outboundModel === '' ? EMPTY : result.outboundModel}</Mono>
            <Typography component="span" variant="caption" sx={{ color: 'text.secondary' }}>
              {t(`router.explain.destination.kind.${result.destination.kind}`, {
                defaultValue: result.destination.kind,
              })}
            </Typography>
          </Box>
          {result.outcome === 'refused' && result.refusal !== undefined ? (
            <Callout
              tone="danger"
              title={t('router.explain.refused.lineDetail', {
                status: result.refusal.status,
                reason: refusalReasonText(result.refusal.reason, t),
              })}
            >
              {t('router.explain.refused.line', {
                status: result.refusal.status,
                reason: refusalReasonText(result.refusal.reason, t),
              })}
              {result.refusal.detail === '' ? '' : ` · ${result.refusal.detail}`}
            </Callout>
          ) : null}
          <details>
            <summary>{t('router.explain.details')}</summary>
            <Box sx={{ pt: spacing.x2 }}>
              <ExplainResultView result={result} />
            </Box>
          </details>
        </Box>
      )}

      <Box sx={{ display: 'flex', alignItems: 'center', gap: 0.5, flexWrap: 'wrap' }}>
        <ContextHelp
          label={t('router.explain.boundary.helpLabel')}
          heading={t('router.explain.boundary.helpHeading')}
        >
          {t('router.explain.boundary.envNoteDetail')}
        </ContextHelp>
        <MuiLink component={RouterLink} to="/settings?tab=environment" variant="caption">
          {t('router.explain.boundary.envLink')}
        </MuiLink>
      </Box>
    </Section>
  );
}
