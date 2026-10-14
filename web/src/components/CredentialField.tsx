// 凭据选择器 + 轮换/撤销（UC-094 A9/A10，web 面）：新建/编辑配置时**按可读名称**选择托管凭据，
// 或就地「添加凭据」一次性录入、生成可读名引用；界面**从不要求用户接触内部引用 ID**
// （`yzagmgr-managed-token://<uid>` 只在提交体里作为 `credRef` 原样传递）。
//
// 契约（`doc/api-v1.md` §4.7）与四条纪律：
// - 列表/创建/读取/轮换/撤销五面**恒不含明文**：展示面只有名称、供应商、掩码与更新时间——
//   「刷新后页面不返回原 key」是结构性成立，本组件也不给任何原值留位置；
// - `cred` 是**一次性明文**，只在「保存凭据 / 提交轮换」这一程存在（成功后组件状态立即清掉）；
//   掩码形态（含 `****` 或恰为 `<已脱敏>`）在录入面先拦（服务端同口径兜底）——掩码不作为新密钥保存；
// - 保存配置**不会**用掩码替换真实凭据：未改动的引用在 PUT 里省略 `credRef`（缺席 = 不改），
//   本组件也不预填任何「看起来像密钥」的替身值；
// - **轮换只接受新值并保持引用身份**（uid/名称/供应商不动，只换 Token 与掩码）；失败
//   （如 `storage`）时**旧凭据原样保留**——旧值仍在展示面、错误如实说明「旧凭据保持不变」；
//   **撤销先展示受影响配置清单**（`GET …/references`）再确认；撤销把凭据置为终态（徽标 + 撤销
//   时刻落地），**任何配置都不被删除或改写**（不静默删配置）；幂等（重复撤销返回同一结果）。

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import type { TFunction } from 'i18next';
import { useTranslation } from 'react-i18next';
import {
  ENDPOINTS,
  createCredential,
  getCredentialReferences,
  listCredentials,
  operationRequestForEndpoint,
  revokeCredential,
  rotateCredential,
} from '../api/endpoints';
import {
  AgmgrApiError,
  toAgmgrApiError,
  type AgmgrApiError as AgmgrApiErrorType,
} from '../api/errors';
import type {
  Credential,
  CredentialList,
  CredentialRevokePayload,
  CredentialReference,
  CredentialReferencesPayload,
} from '../api/types';
import {
  TrackedOperationPreparationError,
  useTrackedOperation,
  type TrackedOperationJob,
} from '../app/UnsavedWorkContext';
import { Callout } from './Callout';
import { ErrorState } from './States';
import { StatusChip } from './StatusChip';
import { TrackedOperationNotice } from './TrackedOperationNotice';
import { formatDateTime } from '../lib/format';
import { secretProblem } from '../lib/profileForm';
import { fontFamily, fontSize, spacing } from '../theme';

type TFn = TFunction<'translation', undefined>;

/** 托管凭据引用的形态前缀（与 `proxy.ArmTokenScheme` 同口径）。 */
const MANAGED_TOKEN_SCHEME = 'yzagmgr-managed-token://';

/**
 * 从引用身份取出末段 uid（rotate/revoke/references 三个端点的路径参数）。
 * 形态不符时返回空串——调用方据此禁用动作，不拼出坏 URL。
 */
export function credentialUid(ref: string): string {
  const trimmed = ref.trim();
  return trimmed.startsWith(MANAGED_TOKEN_SCHEME) ? trimmed.slice(MANAGED_TOKEN_SCHEME.length) : '';
}

/** 一条凭据的可读标签（名称；未命名旧记录给「未命名凭据」，不伪造空名）。 */
export function credentialLabel(credential: Credential, t: TFn): string {
  const name = (credential.name ?? '').trim();
  return name === '' ? t('tool.config.dialog.credential.nameNone') : name;
}

/** 一条凭据的展示摘要（名称 · 供应商 · 掩码 · 更新时间；**不含引用 ID**）。 */
export function credentialSummary(credential: Credential, t: TFn): string {
  const provider = (credential.provider ?? '').trim();
  const parts = [
    credentialLabel(credential, t),
    provider === '' ? t('tool.config.dialog.credential.providerNone') : provider,
    credential.mask,
  ];
  if (credential.updatedAt !== undefined && credential.updatedAt !== '') {
    parts.push(
      t('tool.config.dialog.credential.updatedAt', { at: formatDateTime(credential.updatedAt) }),
    );
  }
  return parts.join(' · ');
}

/** 受影响配置的一条展示摘要（类别 · 归属工具 · 名称；活性由 `active` 徽标承载）。 */
export function referenceSummary(reference: CredentialReference, t: TFn): string {
  const kindKey = `credentials.refKind.${reference.kind}`;
  const kind = t(kindKey);
  const parts = [kind === kindKey ? reference.kind : kind];
  const tool = (reference.toolId ?? '').trim();
  if (tool !== '') parts.push(tool);
  parts.push(reference.name);
  return parts.join(' · ');
}

export interface CredentialFieldProps {
  /** 当前引用（`''` = 未选择；只作为 `credRef` 槽位的值流转）。 */
  value: string;
  onChange: (ref: string) => void;
  /** 文案分流：留空的含义在建卡（不提供）与改卡（保持不变）下不同。 */
  mode: 'create' | 'edit';
  disabled?: boolean;
}

const SECRET_HINT_ID = 'credential-secret-hint';

const ROTATE_SECRET_HINT_ID = 'credential-rotate-secret-hint';

const REVOKE_TITLE_ID = 'credential-revoke-title';

interface CredentialRevokeTarget {
  readonly ref: string;
  readonly uid: string;
  readonly label: string;
}

interface CredentialRevokeReady {
  readonly kind: 'ready';
  readonly target: CredentialRevokeTarget;
  readonly sequence: number;
  readonly references: readonly CredentialReference[];
  readonly changed?: boolean;
}

type CredentialRevokeState =
  | { readonly kind: 'closed' }
  | { readonly kind: 'loading'; readonly target: CredentialRevokeTarget; readonly sequence: number }
  | {
      readonly kind: 'error';
      readonly target: CredentialRevokeTarget;
      readonly sequence: number;
      readonly error: AgmgrApiErrorType;
    }
  | CredentialRevokeReady;

const CLOSED_CREDENTIAL_REVOKE: CredentialRevokeState = { kind: 'closed' };

export function snapshotCredentialReferences(
  references: readonly CredentialReference[],
): readonly CredentialReference[] {
  return Object.freeze(references.map((reference) => Object.freeze({ ...reference })));
}

function credentialReferenceKey(reference: CredentialReference): string {
  const normalizeOptional = (value: string | undefined) => {
    const trimmed = value?.trim() ?? '';
    return trimmed === '' ? null : trimmed;
  };
  return JSON.stringify([
    reference.kind,
    normalizeOptional(reference.toolId),
    normalizeOptional(reference.profileId),
    reference.name,
    reference.active === true,
  ]);
}

/** Compare the complete displayed reference multiset; duplicates matter, order does not. */
export function credentialReferencesMatch(
  left: readonly CredentialReference[],
  right: readonly CredentialReference[],
): boolean {
  if (left.length !== right.length) return false;
  const leftKeys = left.map(credentialReferenceKey).sort();
  const rightKeys = right.map(credentialReferenceKey).sort();
  return leftKeys.every((key, index) => key === rightKeys[index]);
}

export interface CredentialOperationReadback {
  list: CredentialList;
  uid?: string;
  references?: CredentialReferencesPayload;
}

type CredentialReadbackHandler = (
  job: TrackedOperationJob,
  snapshot: CredentialOperationReadback,
) => void;

export const CREDENTIAL_READBACK_DEADLINE_MS = 10_000;

class CredentialReadbackTimeoutError extends Error {
  constructor() {
    super('Credential state readback timed out');
    this.name = 'CredentialReadbackTimeoutError';
  }
}

const credentialOperationUids = new Map<string, string>();
const credentialReadbacksInFlight = new Map<string, Promise<CredentialOperationReadback>>();

/** Keep only the server-assigned identity needed to complete a successful create readback. */
export function rememberCredentialOperationUid(taskId: string, uid: string): void {
  if (taskId !== '' && uid !== '') credentialOperationUids.set(taskId, uid);
}

function loadCredentialOperationReadback(
  taskId: string,
  readbackUid: string | undefined,
): Promise<CredentialOperationReadback> {
  const existing = credentialReadbacksInFlight.get(taskId);
  if (existing !== undefined) return existing;
  const uid = readbackUid ?? credentialOperationUids.get(taskId);
  const controller = new AbortController();
  const expiresAt = Date.now() + CREDENTIAL_READBACK_DEADLINE_MS;
  let active = true;
  let timeout: ReturnType<typeof setTimeout> | undefined;
  const assertActive = () => {
    if (!active || controller.signal.aborted || Date.now() >= expiresAt) {
      active = false;
      controller.abort();
      throw new CredentialReadbackTimeoutError();
    }
  };
  const readback = (async (): Promise<CredentialOperationReadback> => {
    assertActive();
    const list = await listCredentials({ signal: controller.signal });
    assertActive();
    if (uid === undefined) return { list };
    const references = await getCredentialReferences(uid, { signal: controller.signal });
    assertActive();
    return { list, uid, references };
  })();
  const deadline = new Promise<never>((_resolve, reject) => {
    timeout = setTimeout(() => {
      active = false;
      controller.abort();
      reject(new CredentialReadbackTimeoutError());
    }, CREDENTIAL_READBACK_DEADLINE_MS);
  });
  const request = Promise.race([readback, deadline]).finally(() => {
    if (timeout !== undefined) clearTimeout(timeout);
    active = false;
  });
  credentialReadbacksInFlight.set(taskId, request);
  const clearRequest = () => {
    if (credentialReadbacksInFlight.get(taskId) === request) {
      credentialReadbacksInFlight.delete(taskId);
    }
  };
  void request.then(clearRequest, clearRequest);
  return request;
}

/**
 * Shared credential-scope lock and readback surface used by the editor and Settings manager.
 * It reads fresh state before releasing any write lock; retry only repeats GETs, never the write.
 */
export function useCredentialOperationLifecycle(onReadback: CredentialReadbackHandler) {
  const tracked = useTrackedOperation();
  const jobs = tracked.jobsFor('credentials');
  const readbackErrors = useRef(new Map<string, AgmgrApiErrorType>());
  const attemptedReadbacks = useRef(new Set<string>());
  const inFlightReadbacks = useRef(new Map<string, Promise<void>>());
  const [, refreshReadbackErrors] = useState(0);
  const onReadbackRef = useRef(onReadback);
  const mounted = useRef(false);
  onReadbackRef.current = onReadback;

  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);

  const refreshReadback = useCallback(
    (job: TrackedOperationJob): Promise<void> => {
      const existing = inFlightReadbacks.current.get(job.taskId);
      if (existing !== undefined) return existing;
      readbackErrors.current.delete(job.taskId);
      if (mounted.current) refreshReadbackErrors((value) => value + 1);
      const request = (async () => {
        try {
          const readbackUid = job.readback?.kind === 'credentials' ? job.readback.uid : undefined;
          const snapshot = await loadCredentialOperationReadback(job.taskId, readbackUid);
          if (mounted.current) onReadbackRef.current(job, snapshot);
          tracked.confirmReadback(job.taskId);
          credentialOperationUids.delete(job.taskId);
        } catch (err: unknown) {
          readbackErrors.current.set(job.taskId, toAgmgrApiError(err));
          if (mounted.current) refreshReadbackErrors((value) => value + 1);
        }
      })();
      inFlightReadbacks.current.set(job.taskId, request);
      const clearRequest = () => {
        if (inFlightReadbacks.current.get(job.taskId) === request) {
          inFlightReadbacks.current.delete(job.taskId);
        }
      };
      void request.then(clearRequest, clearRequest);
      return request;
    },
    [tracked.confirmReadback],
  );

  useEffect(() => {
    for (const job of jobs) {
      if (!job.needsReadback || job.globalBlocked || attemptedReadbacks.current.has(job.taskId)) {
        continue;
      }
      attemptedReadbacks.current.add(job.taskId);
      void refreshReadback(job);
    }
  }, [jobs, refreshReadback]);

  const retryReadback = useCallback(
    (job: TrackedOperationJob) => {
      attemptedReadbacks.current.add(job.taskId);
      return refreshReadback(job);
    },
    [refreshReadback],
  );

  const scopeLocked = tracked.isScopeLocked('credentials');
  const operationBusy = tracked.isOperationBusy();
  return {
    jobs,
    locked: operationBusy || scopeLocked,
    foreignBusy: operationBusy && !scopeLocked,
    readbackErrors,
    start: tracked.start,
    refreshReadback,
    retryReadback,
    recheck: tracked.recheck,
    acknowledge: tracked.acknowledge,
  };
}

export function CredentialOperationStatuses({
  lifecycle,
}: {
  lifecycle: ReturnType<typeof useCredentialOperationLifecycle>;
}) {
  const { t } = useTranslation();
  return lifecycle.jobs.length === 0 && !lifecycle.foreignBusy ? null : (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }}>
      {lifecycle.foreignBusy ? (
        <Callout tone="warning">{t('operation.lifecycle.running')}</Callout>
      ) : null}
      {lifecycle.jobs.map((job) => {
        const readbackError = lifecycle.readbackErrors.current.get(job.taskId);
        return (
          <Box key={job.taskId}>
            <TrackedOperationNotice
              job={job}
              onRecheck={() => lifecycle.recheck(job.taskId)}
              onReadback={job.needsReadback ? () => lifecycle.retryReadback(job) : undefined}
              onAcknowledge={() => lifecycle.acknowledge(job.taskId)}
            />
            {readbackError === undefined ? null : (
              <ErrorState
                error={readbackError}
                context={t('operation.lifecycle.readbackFailed')}
                compact
              />
            )}
          </Box>
        );
      })}
    </Box>
  );
}

export function CredentialField({ value, onChange, mode, disabled = false }: CredentialFieldProps) {
  const { t } = useTranslation();
  const [items, setItems] = useState<Credential[] | null>(null);
  const [listError, setListError] = useState<AgmgrApiErrorType | null>(null);
  const [reloadTick, setReloadTick] = useState(0);
  const listSequence = useRef(0);

  // 添加凭据（一次性录入）：表单只在展开时存在，收起即清（密钥不留在组件状态里）。
  const [addOpen, setAddOpen] = useState(false);
  const [addName, setAddName] = useState('');
  const [addProvider, setAddProvider] = useState('');
  const [addSecret, setAddSecret] = useState('');
  const [addSubmitted, setAddSubmitted] = useState(false);
  const [saving, setSaving] = useState(false);
  const [addError, setAddError] = useState<AgmgrApiErrorType | null>(null);
  const [savedNote, setSavedNote] = useState('');

  // 轮换（UC-094 A10）：只输入新值；提交后引用/名称不变；失败保留旧凭据（如实呈现）。
  const [rotateOpen, setRotateOpen] = useState(false);
  const [rotateSecret, setRotateSecret] = useState('');
  const [rotateSubmitted, setRotateSubmitted] = useState(false);
  const [rotateSaving, setRotateSaving] = useState(false);
  const [rotateError, setRotateError] = useState<AgmgrApiErrorType | null>(null);
  const [rotateNote, setRotateNote] = useState('');

  // 撤销（UC-094 A10）：先看受影响配置清单（references），确认后撤销；撤销是终态。
  const [revokeState, setRevokeState] = useState<CredentialRevokeState>(CLOSED_CREDENTIAL_REVOKE);
  const revokeStateRef = useRef<CredentialRevokeState>(CLOSED_CREDENTIAL_REVOKE);
  const revokeSequence = useRef(0);
  const [revoking, setRevoking] = useState(false);
  const [revokeChecking, setRevokeChecking] = useState(false);
  const [revokeError, setRevokeError] = useState<AgmgrApiErrorType | null>(null);
  const [revokeNote, setRevokeNote] = useState('');
  /** 撤销播报的完整口径（W5d：短句上屏 + 全文转 title；随 revokeNote 同步置空）。 */
  const [revokeNoteDetail, setRevokeNoteDetail] = useState('');

  const [menuAnchor, setMenuAnchor] = useState<HTMLElement | null>(null);

  const updateRevokeState = (next: CredentialRevokeState) => {
    revokeStateRef.current = next;
    setRevokeState(next);
  };

  const credentialLifecycle = useCredentialOperationLifecycle((job, snapshot) => {
    listSequence.current += 1;
    setItems(snapshot.list.items);
    setListError(null);
    if (job.responseReceived === false) {
      if (job.path === '/credentials') {
        setAddOpen(false);
        setAddName('');
        setAddProvider('');
        setAddSecret('');
        setAddSubmitted(false);
        setAddError(null);
      }
      if (job.path.endsWith('/rotate')) {
        setRotateOpen(false);
        setRotateSecret('');
        setRotateSubmitted(false);
        setRotateError(null);
      }
      if (job.path.endsWith('/revoke')) {
        revokeSequence.current += 1;
        updateRevokeState(CLOSED_CREDENTIAL_REVOKE);
        setRevokeError(null);
      }
    }
  });

  useEffect(() => {
    let alive = true;
    const sequence = ++listSequence.current;
    setItems(null);
    setListError(null);
    void listCredentials().then(
      (data) => {
        if (alive && sequence === listSequence.current) setItems(data.items);
      },
      (err: unknown) => {
        if (alive && sequence === listSequence.current) setListError(toAgmgrApiError(err));
      },
    );
    return () => {
      alive = false;
    };
  }, [reloadTick]);

  const current = useMemo(
    () => (items ?? []).find((item) => item.ref === value) ?? null,
    [items, value],
  );

  /** 当前引用的 uid（rotate/revoke/references 的路径参数）；解析不出 = 动作不可用。 */
  const currentUid = current === null ? '' : credentialUid(current.ref);
  const revokeTarget = revokeState.kind === 'closed' ? null : revokeState.target;
  const revokeLoading = revokeState.kind === 'loading';
  const revokeListError = revokeState.kind === 'error' ? revokeState.error : null;
  const revokeReady = revokeState.kind === 'ready' ? revokeState : null;

  const nameMissing = addName.trim() === '';
  const secretIssue = secretProblem(addSecret);
  const rotateIssue = secretProblem(rotateSecret);

  const submitAdd = () => {
    if (saving || credentialLifecycle.locked) return;
    setAddSubmitted(true);
    setAddError(null);
    if (nameMissing || secretIssue !== null) return;
    setSaving(true);
    const provider = addProvider.trim();
    const route = operationRequestForEndpoint(ENDPOINTS.credentialCreate);
    let taskId = '';
    const attempt = credentialLifecycle.start<Credential>({
      kind: 'apply',
      scope: 'credentials',
      label: 'operation.action.apply',
      readback: { kind: 'credentials' },
      ...route,
      dispatch: async (operation) => {
        const created = await createCredential(
          {
            name: addName.trim(),
            // 供应商留空 = 省略该键（不交付空串属性）。
            ...(provider === '' ? {} : { provider }),
            cred: addSecret,
          },
          operation === undefined ? {} : { operation },
        );
        rememberCredentialOperationUid(taskId, credentialUid(created.ref));
        return created;
      },
      summarize: () => ({ outcome: 'ok' }),
    });
    taskId = attempt.taskId;
    void attempt.promise.then(
      (created) => {
        setSaving(false);
        // 一次性明文随表单收起立即离开组件状态（成功路径不保留 secret）。
        setAddOpen(false);
        setAddName('');
        setAddProvider('');
        setAddSecret('');
        setAddSubmitted(false);
        onChange(created.ref);
        setSavedNote(t('tool.config.dialog.credential.saved', { name: created.name }));
      },
      (err: unknown) => {
        setSaving(false);
        setAddSecret('');
        setAddError(toAgmgrApiError(err));
      },
    );
  };

  const closeAdd = () => {
    setAddOpen(false);
    setAddName('');
    setAddProvider('');
    setAddSecret('');
    setAddSubmitted(false);
    setAddError(null);
  };

  const openRotate = () => {
    setRotateOpen(true);
    setRotateSecret('');
    setRotateSubmitted(false);
    setRotateError(null);
    setRotateNote('');
    setSavedNote('');
  };

  const closeRotate = () => {
    setRotateOpen(false);
    setRotateSecret('');
    setRotateSubmitted(false);
    setRotateError(null);
  };

  /** 轮换提交：只发新值；成功后引用身份不变（服务端回读的新视图直接落列表）。 */
  const submitRotate = () => {
    if (current === null || currentUid === '' || rotateSaving || credentialLifecycle.locked) return;
    setRotateSubmitted(true);
    setRotateError(null);
    if (rotateIssue !== null) return;
    setRotateSaving(true);
    const route = operationRequestForEndpoint(ENDPOINTS.credentialRotate, { uid: currentUid });
    const attempt = credentialLifecycle.start<Credential>({
      kind: 'apply',
      scope: 'credentials',
      label: 'operation.action.apply',
      readback: { kind: 'credentials', uid: currentUid },
      ...route,
      dispatch: (operation) =>
        rotateCredential(
          currentUid,
          { cred: rotateSecret },
          operation === undefined ? {} : { operation },
        ),
      summarize: () => ({ outcome: 'ok' }),
    });
    void attempt.promise.then(
      (rotated) => {
        setRotateSaving(false);
        closeRotate();
        setRotateNote(t('credentials.rotate.done', { name: credentialLabel(rotated, t) }));
      },
      (err: unknown) => {
        setRotateSaving(false);
        setRotateSecret('');
        setRotateError(toAgmgrApiError(err));
      },
    );
  };

  const loadReferences = async (
    target: CredentialRevokeTarget,
    signal?: AbortSignal,
  ): Promise<CredentialRevokeReady | null> => {
    const sequence = ++revokeSequence.current;
    const isCurrent = () => {
      const latest = revokeStateRef.current;
      return latest.kind !== 'closed' && latest.sequence === sequence;
    };
    updateRevokeState({ kind: 'loading', target, sequence });
    const onAbort = () => {
      if (!isCurrent()) return;
      updateRevokeState({
        kind: 'error',
        target,
        sequence,
        error: new AgmgrApiError({
          code: 'internal',
          message: 'Credential reference check was interrupted before dispatch',
        }),
      });
    };
    signal?.addEventListener('abort', onAbort, { once: true });
    try {
      const payload = await getCredentialReferences(
        target.uid,
        signal === undefined ? {} : { signal },
      );
      if (signal?.aborted || !isCurrent()) return null;
      const latest = revokeStateRef.current;
      if (
        latest.kind === 'closed' ||
        latest.target.ref !== target.ref ||
        payload.ref !== target.ref
      ) {
        updateRevokeState({
          kind: 'error',
          target,
          sequence,
          error: new AgmgrApiError({
            code: 'internal',
            message: 'Credential reference response did not match the selected credential',
          }),
        });
        return null;
      }
      const ready: CredentialRevokeReady = {
        kind: 'ready',
        target,
        sequence,
        references: snapshotCredentialReferences(payload.references),
      };
      updateRevokeState(ready);
      return ready;
    } catch (error: unknown) {
      if (!isCurrent()) return null;
      if (!signal?.aborted) {
        updateRevokeState({ kind: 'error', target, sequence, error: toAgmgrApiError(error) });
      }
      return null;
    } finally {
      signal?.removeEventListener('abort', onAbort);
    }
  };

  const openRevoke = () => {
    if (current === null || currentUid === '') return;
    const target = Object.freeze({
      ref: current.ref,
      uid: currentUid,
      label: credentialLabel(current, t),
    });
    setRevokeError(null);
    setRevokeNote('');
    setRevokeNoteDetail('');
    setSavedNote('');
    void loadReferences(target);
  };

  const closeRevoke = () => {
    revokeSequence.current += 1;
    updateRevokeState(CLOSED_CREDENTIAL_REVOKE);
    setRevoking(false);
    setRevokeChecking(false);
    setRevokeError(null);
  };

  const retryRevokeReferences = () => {
    const latest = revokeStateRef.current;
    if (latest.kind !== 'closed') void loadReferences(latest.target);
  };

  /** Confirm is bound to the exact credential and reference snapshot the user just reviewed. */
  const confirmRevoke = (ready: CredentialRevokeReady) => {
    const markLatestListForReview = () => {
      const latest = revokeStateRef.current;
      if (latest.kind === 'ready') updateRevokeState({ ...latest, changed: true });
    };
    if (revoking || credentialLifecycle.locked) return;
    setRevoking(true);
    setRevokeChecking(true);
    setRevokeError(null);
    const { target } = ready;
    const route = operationRequestForEndpoint(ENDPOINTS.credentialRevoke, { uid: target.uid });
    const attempt = credentialLifecycle.start<CredentialRevokePayload>({
      kind: 'apply',
      scope: 'credentials',
      label: 'operation.action.apply',
      readback: { kind: 'credentials', uid: target.uid },
      ...route,
      prepareBeforeDispatch: async (_operation, { signal }) => {
        if (revokeStateRef.current !== ready) {
          markLatestListForReview();
          throw new Error('Credential references changed');
        }
        const fresh = await loadReferences(target, signal);
        if (fresh === null) {
          markLatestListForReview();
          throw new Error('Credential references could not be verified');
        }
        if (!credentialReferencesMatch(ready.references, fresh.references)) {
          updateRevokeState({ ...fresh, changed: true });
          throw new Error('Credential references changed; confirm the updated list');
        }
        setRevokeChecking(false);
      },
      dispatch: (operation) =>
        revokeCredential(target.uid, operation === undefined ? {} : { operation }),
      summarize: () => ({ outcome: 'ok' }),
    });
    void attempt.promise.then(
      (payload) => {
        setRevoking(false);
        setRevokeChecking(false);
        closeRevoke();
        setRevokeNote(
          t('credentials.revoke.done', {
            name: credentialLabel(payload.credential, t),
            total: payload.references.length,
          }),
        );
        // W5d：播报的完整口径（未删除也未改写）转 title。
        setRevokeNoteDetail(
          t('credentials.revoke.doneDetail', {
            name: credentialLabel(payload.credential, t),
            total: payload.references.length,
          }),
        );
      },
      (err: unknown) => {
        setRevoking(false);
        setRevokeChecking(false);
        if (err instanceof TrackedOperationPreparationError) return;
        setRevokeError(toAgmgrApiError(err));
      },
    );
  };

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5, minWidth: 0 }}>
      <CredentialOperationStatuses lifecycle={credentialLifecycle} />
      <Typography component="span" variant="caption" sx={{ color: 'text.secondary' }}>
        {t('tool.config.dialog.credential.label')}
      </Typography>

      {/* 当前值：能解析出记录就按摘要呈现；解析不出也**不展示内部 ID**（不要求用户理解引用）。
          「已保存 / 未提供 / 已撤销」三态明确区分：撤销的引用带撤销徽标与撤销时刻
          （不改选、不隐藏；撤销是终态，轮换/撤销入口随之收起）。 */}
      {value === '' ? (
        <Typography component="p" variant="caption" sx={{ color: 'text.secondary' }}>
          {mode === 'create'
            ? t('tool.config.dialog.credential.none.create')
            : t('tool.config.dialog.credential.none.edit')}
        </Typography>
      ) : current !== null ? (
        <Box sx={{ display: 'flex', alignItems: 'center', gap: spacing.x1, flexWrap: 'wrap' }}>
          <Typography
            component="span"
            variant="body2"
            sx={{ fontFamily: fontFamily.mono, fontSize: fontSize.mono, overflowWrap: 'anywhere' }}
          >
            {credentialSummary(current, t)}
          </Typography>
          {current.revoked === true ? (
            <>
              <StatusChip tone="danger" label={t('credentials.revokedBadge')} />
              {current.revokedAt !== undefined && current.revokedAt !== '' ? (
                <Typography component="span" variant="caption" sx={{ color: 'text.secondary' }}>
                  {t('credentials.revokedAt', { at: formatDateTime(current.revokedAt) })}
                </Typography>
              ) : null}
            </>
          ) : null}
        </Box>
      ) : items === null ? (
        <Typography component="p" variant="caption" sx={{ color: 'text.secondary' }}>
          {t('tool.config.dialog.credential.loading')}
        </Typography>
      ) : (
        <Typography
          component="p"
          variant="caption"
          title={t('tool.config.dialog.credential.unresolvedDetail')}
          sx={{ color: 'text.secondary' }}
        >
          {t('tool.config.dialog.credential.unresolved')}
        </Typography>
      )}

      <Box sx={{ display: 'flex', alignItems: 'center', gap: spacing.x1, flexWrap: 'wrap' }}>
        <Button
          type="button"
          size="small"
          variant="outlined"
          disabled={disabled || credentialLifecycle.locked || items === null || items.length === 0}
          aria-haspopup="menu"
          aria-label={t('tool.config.dialog.credential.selectAria', {
            current:
              current === null
                ? t('tool.config.dialog.credential.noneShort')
                : credentialLabel(current, t),
          })}
          onClick={(event) => setMenuAnchor(event.currentTarget)}
        >
          {t('tool.config.dialog.credential.select')}
        </Button>
        <Button
          type="button"
          size="small"
          variant="text"
          disabled={disabled || credentialLifecycle.locked}
          onClick={() => {
            setSavedNote('');
            setAddOpen(true);
          }}
        >
          {t('tool.config.dialog.credential.add')}
        </Button>
        {/* UC-094 A10：轮换/撤销只在当前值可解析（有可读记录）且未撤销时提供——
            撤销是终态（服务端拒绝已撤销凭据的轮换），界面不摆无效入口。 */}
        {current !== null && current.revoked !== true && currentUid !== '' ? (
          <>
            <Button
              type="button"
              size="small"
              variant="text"
              disabled={disabled || credentialLifecycle.locked}
              onClick={openRotate}
            >
              {t('credentials.rotate.action')}
            </Button>
            <Button
              type="button"
              size="small"
              variant="text"
              disabled={disabled || credentialLifecycle.locked}
              onClick={openRevoke}
            >
              {t('credentials.revoke.action')}
            </Button>
          </>
        ) : null}
      </Box>

      <Menu
        anchorEl={menuAnchor}
        open={menuAnchor !== null}
        onClose={() => setMenuAnchor(null)}
        slotProps={{ list: { 'aria-label': t('tool.config.dialog.credential.select') } }}
      >
        {(items ?? []).map((item) => (
          <MenuItem
            key={item.ref}
            selected={item.ref === value}
            onClick={() => {
              setMenuAnchor(null);
              setSavedNote('');
              onChange(item.ref);
            }}
          >
            <Box sx={{ display: 'flex', alignItems: 'center', gap: spacing.x1, flexWrap: 'wrap' }}>
              <Typography
                component="span"
                variant="body2"
                sx={{ fontFamily: fontFamily.mono, fontSize: fontSize.mono }}
              >
                {credentialSummary(item, t)}
              </Typography>
              {item.revoked === true ? (
                <StatusChip tone="danger" label={t('credentials.revokedBadge')} />
              ) : null}
            </Box>
          </MenuItem>
        ))}
      </Menu>

      {savedNote === '' ? null : (
        <Typography component="p" variant="caption" sx={{ color: 'text.secondary' }} role="status">
          {savedNote}
        </Typography>
      )}

      {rotateNote === '' ? null : (
        <Typography component="p" variant="caption" sx={{ color: 'text.secondary' }} role="status">
          {rotateNote}
        </Typography>
      )}

      {revokeNote === '' ? null : (
        <Typography
          component="p"
          variant="caption"
          role="status"
          title={revokeNoteDetail}
          sx={{ color: 'text.secondary' }}
        >
          {revokeNote}
        </Typography>
      )}

      {addOpen ? (
        <Box
          sx={{
            display: 'flex',
            flexDirection: 'column',
            gap: spacing.x1,
            pt: spacing.x1,
            borderTop: '1px solid',
            borderColor: 'divider',
          }}
        >
          <Typography component="p" variant="caption" sx={{ fontWeight: 600 }}>
            {t('tool.config.dialog.credential.addTitle')}
          </Typography>
          <Typography
            component="p"
            variant="caption"
            title={t('tool.config.dialog.credential.addNoteDetail')}
            sx={{ color: 'text.secondary' }}
          >
            {t('tool.config.dialog.credential.addNote')}
          </Typography>
          <TextField
            size="small"
            fullWidth
            required
            label={t('tool.config.dialog.credential.nameLabel')}
            value={addName}
            disabled={credentialLifecycle.locked}
            error={addSubmitted && nameMissing}
            helperText={
              addSubmitted && nameMissing
                ? t('tool.config.dialog.credential.nameRequired')
                : undefined
            }
            onChange={(event) => setAddName(event.target.value)}
          />
          <TextField
            size="small"
            fullWidth
            label={t('tool.config.dialog.credential.providerLabel')}
            value={addProvider}
            disabled={credentialLifecycle.locked}
            onChange={(event) => setAddProvider(event.target.value)}
          />
          <TextField
            size="small"
            fullWidth
            required
            type="password"
            autoComplete="off"
            label={t('tool.config.dialog.credential.secretLabel')}
            value={addSecret}
            disabled={credentialLifecycle.locked}
            error={addSubmitted && secretIssue !== null}
            aria-describedby={SECRET_HINT_ID}
            helperText={
              addSubmitted && secretIssue !== null
                ? secretIssue === 'required'
                  ? t('tool.config.dialog.credential.secretRequired')
                  : secretIssue === 'mask'
                    ? t('tool.config.dialog.credential.secretMask')
                    : t('tool.config.dialog.credential.secretControl')
                : undefined
            }
            onChange={(event) => setAddSecret(event.target.value)}
          />
          <Typography
            id={SECRET_HINT_ID}
            component="p"
            variant="caption"
            sx={{ color: 'text.secondary' }}
          >
            {t('tool.config.dialog.credential.secretHint')}
          </Typography>
          {addError === null ? null : (
            <ErrorState
              error={addError}
              context={t('tool.config.dialog.credential.addErrorContext')}
              compact
            />
          )}
          <Box sx={{ display: 'flex', gap: spacing.x1 }}>
            <Button type="button" size="small" variant="text" onClick={closeAdd}>
              {t('common.cancel')}
            </Button>
            <Button
              type="button"
              size="small"
              variant="contained"
              disabled={saving || credentialLifecycle.locked}
              onClick={submitAdd}
            >
              {saving
                ? t('tool.config.dialog.credential.saving')
                : t('tool.config.dialog.credential.submit')}
            </Button>
          </Box>
        </Box>
      ) : null}

      {/* UC-094 A10 轮换：只输入新值；引用身份/名称/供应商不变；失败保留旧凭据。 */}
      {rotateOpen ? (
        <Box
          sx={{
            display: 'flex',
            flexDirection: 'column',
            gap: spacing.x1,
            pt: spacing.x1,
            borderTop: '1px solid',
            borderColor: 'divider',
          }}
        >
          <Typography component="p" variant="caption" sx={{ fontWeight: 600 }}>
            {t('credentials.rotate.title')}
          </Typography>
          <Typography
            component="p"
            variant="caption"
            title={t('credentials.rotate.noteDetail')}
            sx={{ color: 'text.secondary' }}
          >
            {t('credentials.rotate.note')}
          </Typography>
          <TextField
            size="small"
            fullWidth
            required
            type="password"
            autoComplete="off"
            label={t('credentials.rotate.secretLabel')}
            value={rotateSecret}
            disabled={credentialLifecycle.locked}
            error={rotateSubmitted && rotateIssue !== null}
            aria-describedby={ROTATE_SECRET_HINT_ID}
            helperText={
              rotateSubmitted && rotateIssue !== null
                ? rotateIssue === 'required'
                  ? t('tool.config.dialog.credential.secretRequired')
                  : rotateIssue === 'mask'
                    ? t('tool.config.dialog.credential.secretMask')
                    : t('tool.config.dialog.credential.secretControl')
                : undefined
            }
            onChange={(event) => setRotateSecret(event.target.value)}
          />
          <Typography
            id={ROTATE_SECRET_HINT_ID}
            component="p"
            variant="caption"
            sx={{ color: 'text.secondary' }}
          >
            {t('tool.config.dialog.credential.secretHint')}
          </Typography>
          {rotateError === null ? null : (
            <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5, minWidth: 0 }}>
              {/* 失败如实呈现：错误原文 + 明确口径「旧凭据保持不变」——且旧掩码仍在上方展示面。 */}
              <Typography
                component="p"
                variant="caption"
                sx={{ color: 'text.secondary', overflowWrap: 'anywhere' }}
              >
                {rotateError.message}
              </Typography>
              <Typography component="p" variant="caption" role="status">
                {t('credentials.rotate.failedNote')}
              </Typography>
              <ErrorState
                error={rotateError}
                context={t('credentials.rotate.errorContext')}
                compact
              />
            </Box>
          )}
          <Box sx={{ display: 'flex', gap: spacing.x1 }}>
            <Button type="button" size="small" variant="text" onClick={closeRotate}>
              {t('common.cancel')}
            </Button>
            <Button
              type="button"
              size="small"
              variant="contained"
              disabled={rotateSaving || credentialLifecycle.locked}
              onClick={submitRotate}
            >
              {rotateSaving ? t('credentials.rotate.submitting') : t('credentials.rotate.submit')}
            </Button>
          </Box>
        </Box>
      ) : null}

      {/* UC-094 A10 撤销确认：先展示 references 列出的受影响配置清单，确认后撤销；
          清单未读出前不可确认；文案明说「不静默删配置」与幂等。 */}
      <Dialog
        open={revokeTarget !== null}
        onClose={closeRevoke}
        maxWidth="sm"
        fullWidth
        aria-labelledby={REVOKE_TITLE_ID}
      >
        <DialogTitle id={REVOKE_TITLE_ID} sx={{ fontSize: 14, fontWeight: 620 }}>
          {t('credentials.revoke.title', { name: revokeTarget?.label ?? '' })}
        </DialogTitle>
        <DialogContent sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x1 }}>
          <CredentialOperationStatuses lifecycle={credentialLifecycle} />
          <Typography component="p" variant="body2">
            {t('credentials.revoke.intro')}
          </Typography>
          <Typography component="p" variant="caption" sx={{ color: 'text.secondary' }}>
            {t('credentials.revoke.noSilentDelete')}
          </Typography>
          {revokeLoading ? (
            <Typography component="p" variant="caption" sx={{ color: 'text.secondary' }}>
              {t('credentials.revoke.referencesLoading')}
            </Typography>
          ) : revokeListError !== null ? (
            <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5, minWidth: 0 }}>
              <Typography component="p" variant="caption" sx={{ color: 'text.secondary' }}>
                {t('credentials.revoke.referencesFailed')}
              </Typography>
              <ErrorState
                error={revokeListError}
                context={t('credentials.revoke.referencesContext')}
                retry={retryRevokeReferences}
                compact
              />
            </Box>
          ) : revokeReady === null ? null : (
            <>
              <Typography component="p" variant="caption" sx={{ fontWeight: 620 }}>
                {t('credentials.revoke.referencesTitle', {
                  total: revokeReady.references.length,
                })}
              </Typography>
              {revokeReady.references.length === 0 ? (
                <Typography component="p" variant="caption" sx={{ color: 'text.secondary' }}>
                  {t('credentials.revoke.referencesEmpty')}
                </Typography>
              ) : (
                <Box component="ul" sx={{ listStyle: 'none', m: 0, p: 0, minWidth: 0 }}>
                  {revokeReady.references.map((reference, index) => (
                    <Box
                      component="li"
                      key={`${reference.kind}:${reference.toolId ?? ''}:${reference.profileId ?? ''}:${reference.name}:${index}`}
                      sx={{
                        display: 'flex',
                        alignItems: 'center',
                        gap: spacing.x1,
                        flexWrap: 'wrap',
                      }}
                    >
                      <Typography
                        component="span"
                        variant="body2"
                        sx={{ fontFamily: fontFamily.mono, fontSize: fontSize.mono }}
                      >
                        {referenceSummary(reference, t)}
                      </Typography>
                      {reference.active === true ? (
                        <StatusChip tone="warning" label={t('credentials.refActive')} />
                      ) : null}
                    </Box>
                  ))}
                </Box>
              )}
            </>
          )}
          {revokeReady?.changed === true ? (
            <Callout tone="warning">{t('credentials.revoke.referencesChanged')}</Callout>
          ) : null}
          {revokeError === null ? null : (
            <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5, minWidth: 0 }}>
              <Typography
                component="p"
                variant="caption"
                sx={{ color: 'text.secondary', overflowWrap: 'anywhere' }}
              >
                {revokeError.message}
              </Typography>
              <ErrorState
                error={revokeError}
                context={t('credentials.revoke.errorContext')}
                compact
              />
            </Box>
          )}
        </DialogContent>
        <DialogActions sx={{ px: 3, pb: 2 }}>
          <Button type="button" variant="text" disabled={revoking} onClick={closeRevoke}>
            {t('common.cancel')}
          </Button>
          <Button
            type="button"
            variant="contained"
            color="error"
            disabled={revoking || credentialLifecycle.locked || revokeReady === null}
            onClick={revokeReady === null ? undefined : () => confirmRevoke(revokeReady)}
          >
            {revokeChecking
              ? t('credentials.revoke.checking')
              : revoking
                ? t('credentials.revoke.revoking')
                : t('credentials.revoke.confirm')}
          </Button>
        </DialogActions>
      </Dialog>

      {listError === null ? null : (
        <ErrorState
          error={listError}
          context={t('tool.config.dialog.credential.loadFailed')}
          retry={() => setReloadTick((tick) => tick + 1)}
          compact
        />
      )}
      {items !== null && items.length === 0 && !addOpen ? (
        <Typography component="p" variant="caption" sx={{ color: 'text.secondary' }}>
          {t('tool.config.dialog.credential.empty')}
        </Typography>
      ) : null}
    </Box>
  );
}

export default CredentialField;
