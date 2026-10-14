import { useEffect, useState } from 'react';
import Button from '@mui/material/Button';
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import { useTranslation } from 'react-i18next';
import { useTrackedOperation, type TrackedOperationJob } from '../app/UnsavedWorkContext';
import { layout } from '../theme';
import { Callout } from './Callout';

export interface TrackedOperationNoticeProps {
  job: TrackedOperationJob;
  onRecheck?: () => void | Promise<unknown>;
  onReadback?: () => void | Promise<unknown>;
  onAcknowledge?: () => void;
}

/** Cross-route status surface for an operation whose original view may have closed. */
export function TrackedOperationNotice({
  job,
  onRecheck,
  onReadback,
  onAcknowledge,
}: TrackedOperationNoticeProps) {
  const { t } = useTranslation();
  const { registerNoticeVisible } = useTrackedOperation();
  const [pending, setPending] = useState(false);
  const invoke = (action: (() => void | Promise<unknown>) | undefined) => {
    if (action === undefined || pending) return;
    setPending(true);
    void Promise.resolve()
      .then(action)
      .catch(() => undefined)
      .finally(() => setPending(false));
  };

  let messageKey: string | null = null;
  let testId = 'tracked-operation-status';
  let action: (() => void | Promise<unknown>) | undefined;
  let actionLabelKey: string | undefined;
  let allowDismiss = false;
  let tone: 'info' | 'warning' | 'danger' = 'info';

  if (job.phase === 'reserving' || job.phase === 'reserved') {
    messageKey = 'operation.lifecycle.reserving';
  } else if (job.phase === 'running') {
    messageKey = 'operation.lifecycle.running';
    action = onRecheck;
    actionLabelKey = 'operation.lifecycle.recheck';
  } else if (job.phase === 'reserve_unknown') {
    messageKey = 'operation.lifecycle.reserveUnknown';
    testId = 'tracked-operation-reserve-cooldown';
    tone = 'warning';
  } else if (job.phase === 'reserve_failed') {
    messageKey = 'operation.lifecycle.reserveFailed';
    tone = 'danger';
    allowDismiss = true;
  } else if (job.phase === 'reserve_expired') {
    messageKey = 'operation.lifecycle.reserveExpired';
    tone = 'info';
    allowDismiss = true;
  } else if (job.phase === 'closed') {
    messageKey = 'operation.lifecycle.noDispatch';
    testId = 'tracked-operation-reserve-cooldown';
    allowDismiss = true;
  } else if (job.phase === 'unknown') {
    messageKey =
      job.reason === 'identity_mismatch'
        ? 'operation.lifecycle.identityMismatch'
        : job.reason === 'invalid_operation_status'
          ? 'operation.lifecycle.statusUntrusted'
          : 'operation.lifecycle.resultUnknown';
    tone = 'warning';
    action = onRecheck;
    actionLabelKey = 'operation.lifecycle.recheck';
  } else if (job.phase === 'fenced_unknown' || job.needsReadback) {
    if (job.needsReadback) {
      messageKey = 'operation.lifecycle.readbackRequired';
      testId = 'tracked-operation-readback-lock';
      tone = 'warning';
      action = onReadback;
      actionLabelKey = 'operation.lifecycle.refresh';
    } else {
      messageKey = 'operation.lifecycle.resultUnknown';
      tone = 'warning';
      allowDismiss = true;
    }
  } else if (job.summary?.outcome === 'unconfirmed') {
    messageKey = 'operation.lifecycle.resultUnknown';
    tone = 'warning';
    allowDismiss = true;
  } else if (job.phase === 'completed' && !job.responseReceived) {
    messageKey = 'operation.lifecycle.resultUnknown';
    tone = 'warning';
    allowDismiss = true;
  } else if (job.phase === 'completed') {
    const terminalError =
      job.summary?.errorCode !== undefined ||
      (job.summary?.httpStatus !== undefined && job.summary.httpStatus >= 400);
    messageKey = terminalError
      ? 'operation.lifecycle.terminalError'
      : 'operation.lifecycle.completed';
    tone = terminalError ? 'danger' : 'info';
    allowDismiss = true;
  }

  const messageVisible = messageKey !== null;
  useEffect(() => {
    if (!messageVisible) return;
    return registerNoticeVisible(job.taskId);
  }, [job.taskId, messageVisible, registerNoticeVisible]);

  if (messageKey === null) return null;

  return (
    <Box data-testid={testId} role="status" aria-live="polite">
      <Callout
        tone={tone}
        action={
          action === undefined && !(allowDismiss && onAcknowledge !== undefined) ? undefined : (
            <Box sx={{ display: 'flex', alignItems: 'center', gap: 0.5, flexWrap: 'wrap' }}>
              {action === undefined || actionLabelKey === undefined ? null : (
                <Button
                  type="button"
                  size="small"
                  variant="outlined"
                  disabled={pending}
                  data-testid={
                    testId === 'tracked-operation-readback-lock'
                      ? 'tracked-operation-readback'
                      : 'tracked-operation-recheck'
                  }
                  onClick={() => invoke(action)}
                  sx={{ minHeight: layout.touchTargetMinimum }}
                >
                  {t(actionLabelKey)}
                </Button>
              )}
              {allowDismiss && onAcknowledge !== undefined ? (
                <Button
                  type="button"
                  size="small"
                  variant="text"
                  data-testid="tracked-operation-dismiss"
                  onClick={onAcknowledge}
                  sx={{ minHeight: layout.touchTargetMinimum }}
                >
                  {t('operation.lifecycle.dismiss')}
                </Button>
              ) : null}
            </Box>
          )
        }
      >
        <>
          {t(messageKey)}
          {job.summary?.outcome === 'applied' ? ` · ${t('tool.action.result.applied')}` : ''}
          {job.summary?.outcome === 'unchanged' ? ` · ${t('tool.action.result.unchanged')}` : ''}
        </>
      </Callout>
      {job.summary?.errorCode === undefined ? null : (
        <Typography component="p" variant="caption" sx={{ mt: 0.5, color: 'text.secondary' }}>
          {t('error.codeLine', { code: job.summary.errorCode })}
        </Typography>
      )}
      {job.summary?.backupIds === undefined || job.summary.backupIds.length === 0 ? null : (
        <Typography component="p" variant="caption" sx={{ mt: 0.5, color: 'text.secondary' }}>
          {t('operation.lifecycle.backups', { backups: job.summary.backupIds.join(', ') })}
        </Typography>
      )}
    </Box>
  );
}
