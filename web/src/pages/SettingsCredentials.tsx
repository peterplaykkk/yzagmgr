import { useCallback, useId, useRef, useState } from 'react';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
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
import type { Credential, CredentialReference, CredentialRevokePayload } from '../api/types';
import {
  TrackedOperationPreparationError,
  type TrackedOperationJob,
} from '../app/UnsavedWorkContext';
import { ContextHelp } from '../components/ContextHelp';
import { Callout } from '../components/Callout';
import { EmptyState, ErrorState, LoadingBlock } from '../components/States';
import { Section } from '../components/Section';
import { StatusChip } from '../components/StatusChip';
import { TableScroll } from '../components/TableScroll';
import {
  CredentialOperationStatuses,
  credentialUid,
  rememberCredentialOperationUid,
  snapshotCredentialReferences,
  useCredentialOperationLifecycle,
  credentialReferencesMatch,
  type CredentialOperationReadback,
} from '../components/CredentialField';
import { formatDateTime } from '../lib/format';
import { secretProblem } from '../lib/profileForm';
import { useResource } from '../lib/useResource';
import { color, fontFamily, fontSize, fontWeight, spacing } from '../theme';

type ReferenceState =
  | {
      readonly kind: 'loading';
      readonly ref: string;
      readonly uid: string;
      readonly sequence: number;
    }
  | {
      readonly kind: 'ready';
      readonly ref: string;
      readonly uid: string;
      readonly sequence: number;
      readonly items: readonly CredentialReference[];
      readonly changed?: boolean;
    }
  | {
      readonly kind: 'error';
      readonly ref: string;
      readonly uid: string;
      readonly sequence: number;
      readonly error: AgmgrApiErrorType;
    };

interface CredentialRevokeTarget {
  readonly credential: Credential;
  readonly ref: string;
  readonly uid: string;
}

function CredentialReferences({
  credential,
  state,
  onLoad,
}: {
  credential: Credential;
  state: ReferenceState | undefined;
  onLoad: (credential: Credential) => void;
}) {
  const { t } = useTranslation();
  const name = (credential.name ?? '').trim() || t('tool.config.dialog.credential.nameNone');
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x2, minWidth: 0 }}>
      {state?.kind === 'loading' ? (
        <Typography
          component="p"
          role="status"
          variant="caption"
          sx={{ m: 0, color: 'text.secondary' }}
        >
          {t('credentials.revoke.referencesLoading')}
        </Typography>
      ) : null}
      {state?.kind !== 'error' ? null : (
        <ErrorState
          error={state.error}
          context={t('settings.credentials.referencesError')}
          compact
          retry={() => onLoad(credential)}
        />
      )}
      {state?.kind !== 'ready' ? null : state.items.length === 0 ? (
        <Typography component="p" variant="caption" sx={{ m: 0, color: 'text.secondary' }}>
          {t('credentials.revoke.referencesEmpty')}
        </Typography>
      ) : (
        <Box
          component="ul"
          sx={{
            m: 0,
            p: 0,
            listStyle: 'none',
            display: 'flex',
            flexDirection: 'column',
            gap: spacing.x1,
          }}
        >
          {state.items.map((reference, index) => (
            <Box
              component="li"
              key={`${reference.kind}-${reference.toolId ?? ''}-${reference.profileId ?? ''}-${index}`}
              sx={{ display: 'flex', alignItems: 'center', gap: spacing.x2, flexWrap: 'wrap' }}
            >
              <Typography component="span" variant="body2" sx={{ fontWeight: fontWeight.medium }}>
                {reference.name || name}
              </Typography>
              <StatusChip
                tone="neutral"
                label={t(`credentials.refKind.${reference.kind}`, { defaultValue: reference.kind })}
              />
              {reference.toolId === undefined ? null : (
                <Typography component="span" variant="caption" sx={{ color: 'text.secondary' }}>
                  {reference.toolId}
                </Typography>
              )}
              {reference.active ? (
                <StatusChip tone="warning" label={t('credentials.refActive')} />
              ) : null}
            </Box>
          ))}
        </Box>
      )}
    </Box>
  );
}

function CredentialRow({
  credential,
  referenceState,
  expanded,
  busy,
  onToggleReferences,
  onLoadReferences,
  onRotate,
  onRevoke,
}: {
  credential: Credential;
  referenceState: ReferenceState | undefined;
  expanded: boolean;
  busy: boolean;
  onToggleReferences: (credential: Credential) => void;
  onLoadReferences: (credential: Credential) => void;
  onRotate: (credential: Credential) => void;
  onRevoke: (credential: Credential) => void;
}) {
  const { t } = useTranslation();
  const name = (credential.name ?? '').trim() || t('tool.config.dialog.credential.nameNone');
  const provider =
    (credential.provider ?? '').trim() || t('tool.config.dialog.credential.providerNone');
  const uid = credentialUid(credential.ref);
  const referencesId = `credential-references-${useId().replace(/[^a-zA-Z0-9_-]/g, '')}`;
  return (
    <>
      <Box
        component="tr"
        data-credential-row="true"
        sx={{
          borderTop: `1px solid ${color.borderDivider}`,
          '&:hover': { backgroundColor: color.hoverOverlay },
        }}
      >
        <Box
          component="td"
          sx={{ px: spacing.x2, py: spacing.x2, verticalAlign: 'top', minWidth: 160 }}
        >
          <Typography
            component="span"
            variant="body2"
            sx={{ fontWeight: fontWeight.semibold, overflowWrap: 'anywhere' }}
          >
            {name}
          </Typography>
        </Box>
        <Box component="td" sx={{ px: spacing.x2, py: spacing.x2, verticalAlign: 'top' }}>
          <Typography component="span" variant="body2">
            {provider}
          </Typography>
        </Box>
        <Box component="td" sx={{ px: spacing.x2, py: spacing.x2, verticalAlign: 'top' }}>
          <Typography
            component="span"
            sx={{ fontFamily: fontFamily.mono, fontSize: fontSize.mono }}
          >
            {credential.mask}
          </Typography>
        </Box>
        <Box
          component="td"
          sx={{ px: spacing.x2, py: spacing.x2, verticalAlign: 'top', whiteSpace: 'nowrap' }}
        >
          {credential.updatedAt === undefined ? '—' : formatDateTime(credential.updatedAt)}
        </Box>
        <Box component="td" sx={{ px: spacing.x2, py: spacing.x2, verticalAlign: 'top' }}>
          <StatusChip
            tone={credential.revoked ? 'danger' : 'success'}
            label={
              credential.revoked
                ? t('credentials.revokedBadge')
                : t('settings.credentials.available')
            }
          />
        </Box>
        <Box component="td" sx={{ px: spacing.x2, py: spacing.x2, verticalAlign: 'top' }}>
          <Button
            type="button"
            size="small"
            variant="text"
            aria-expanded={expanded}
            aria-controls={expanded ? referencesId : undefined}
            onClick={() => onToggleReferences(credential)}
          >
            {expanded
              ? t('settings.credentials.hideReferences')
              : t('settings.credentials.viewReferences')}
          </Button>
        </Box>
        <Box
          component="td"
          data-credential-column="actions"
          sx={{
            position: 'sticky',
            right: 0,
            zIndex: 1,
            minWidth: 144,
            px: spacing.x2,
            py: spacing.x2,
            verticalAlign: 'top',
            backgroundColor: color.surface,
            borderLeft: `1px solid ${color.borderDivider}`,
          }}
        >
          <Box sx={{ display: 'flex', alignItems: 'center', gap: spacing.x1, flexWrap: 'wrap' }}>
            <Button
              type="button"
              size="small"
              variant="text"
              disabled={busy || credential.revoked === true || uid === ''}
              onClick={() => onRotate(credential)}
            >
              {t('credentials.rotate.action')}
            </Button>
            <Button
              type="button"
              size="small"
              variant="text"
              color="error"
              disabled={busy || credential.revoked === true || uid === ''}
              onClick={() => onRevoke(credential)}
            >
              {t('credentials.revoke.action')}
            </Button>
          </Box>
        </Box>
      </Box>
      {expanded ? (
        <Box component="tr" id={referencesId}>
          <Box
            component="td"
            colSpan={7}
            sx={{ px: spacing.x3, py: spacing.x2, backgroundColor: color.bgSubtle }}
          >
            <Typography
              component="h3"
              variant="body2"
              sx={{ mb: spacing.x1, fontWeight: fontWeight.semibold }}
            >
              {t('credentials.revoke.referencesTitle', {
                total: referenceState?.kind === 'ready' ? referenceState.items.length : '—',
              })}
            </Typography>
            <CredentialReferences
              credential={credential}
              state={referenceState}
              onLoad={onLoadReferences}
            />
          </Box>
        </Box>
      ) : null}
    </>
  );
}

export function SettingsCredentials() {
  const { t } = useTranslation();
  const credentials = useResource((signal) => listCredentials({ signal }), {
    deps: [],
  });
  const [references, setReferences] = useState<Record<string, ReferenceState>>({});
  const referenceSequenceRef = useRef(new Map<string, number>());
  const [expandedRef, setExpandedRef] = useState('');
  const [addOpen, setAddOpen] = useState(false);
  const [addName, setAddName] = useState('');
  const [addProvider, setAddProvider] = useState('');
  const [addSecret, setAddSecret] = useState('');
  const [addSubmitted, setAddSubmitted] = useState(false);
  const [addError, setAddError] = useState<AgmgrApiErrorType | null>(null);
  const [saving, setSaving] = useState(false);
  const [rotateTarget, setRotateTarget] = useState<Credential | null>(null);
  const [rotateSecret, setRotateSecret] = useState('');
  const [rotateSubmitted, setRotateSubmitted] = useState(false);
  const [rotateError, setRotateError] = useState<AgmgrApiErrorType | null>(null);
  const [rotating, setRotating] = useState(false);
  const [revokeTarget, setRevokeTarget] = useState<CredentialRevokeTarget | null>(null);
  const [revokeError, setRevokeError] = useState<AgmgrApiErrorType | null>(null);
  const [revoking, setRevoking] = useState(false);
  const [revokeChecking, setRevokeChecking] = useState(false);
  const [notice, setNotice] = useState('');

  const commitReferenceState = useCallback((ref: string, state: ReferenceState) => {
    setReferences((current) => ({ ...current, [ref]: state }));
  }, []);

  const nextReferenceSequence = useCallback((ref: string) => {
    const sequence = (referenceSequenceRef.current.get(ref) ?? 0) + 1;
    referenceSequenceRef.current.set(ref, sequence);
    return sequence;
  }, []);

  const isCurrentReferenceSequence = useCallback((ref: string, sequence: number) => {
    return referenceSequenceRef.current.get(ref) === sequence;
  }, []);

  const onCredentialReadback = useCallback(
    (job: TrackedOperationJob, snapshot: CredentialOperationReadback) => {
      credentials.setData(snapshot.list);
      const referenceItems = snapshot.references?.references;
      if (snapshot.uid !== undefined && referenceItems !== undefined) {
        const updated = snapshot.list.items.find(
          (item) => credentialUid(item.ref) === snapshot.uid,
        );
        if (updated !== undefined && snapshot.references?.ref === updated.ref) {
          const sequence = nextReferenceSequence(updated.ref);
          commitReferenceState(updated.ref, {
            kind: 'ready',
            ref: updated.ref,
            uid: snapshot.uid,
            sequence,
            items: snapshotCredentialReferences(referenceItems),
          });
        }
      }
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
          setRotateTarget(null);
          setRotateSecret('');
          setRotateSubmitted(false);
          setRotateError(null);
        }
        if (job.path.endsWith('/revoke')) {
          setRevokeTarget(null);
          setRevoking(false);
          setRevokeChecking(false);
          setRevokeError(null);
        }
      }
    },
    [commitReferenceState, credentials.setData, nextReferenceSequence],
  );
  const credentialLifecycle = useCredentialOperationLifecycle(onCredentialReadback);

  const items = credentials.data?.items ?? [];
  const addSecretProblem = secretProblem(addSecret);
  const rotateSecretProblem = secretProblem(rotateSecret);
  const nameProblem = addName.trim() === '' || addName.trim().length > 100;

  const loadReferences = useCallback(
    async (
      credential: Credential,
      signal?: AbortSignal,
    ): Promise<Extract<ReferenceState, { kind: 'ready' }> | null> => {
      const uid = credentialUid(credential.ref);
      if (uid === '') return null;
      const ref = credential.ref;
      const sequence = nextReferenceSequence(ref);
      const isCurrent = () => isCurrentReferenceSequence(ref, sequence);
      commitReferenceState(ref, { kind: 'loading', ref, uid, sequence });
      const onAbort = () => {
        if (!isCurrent()) return;
        commitReferenceState(ref, {
          kind: 'error',
          ref,
          uid,
          sequence,
          error: new AgmgrApiError({
            code: 'internal',
            message: 'Credential reference check was interrupted before dispatch',
          }),
        });
      };
      signal?.addEventListener('abort', onAbort, { once: true });
      try {
        const payload = await getCredentialReferences(uid, signal === undefined ? {} : { signal });
        if (signal?.aborted || !isCurrent()) return null;
        if (payload.ref !== ref) {
          commitReferenceState(ref, {
            kind: 'error',
            ref,
            uid,
            sequence,
            error: new AgmgrApiError({
              code: 'internal',
              message: 'Credential reference response did not match the requested credential',
            }),
          });
          return null;
        }
        const ready: Extract<ReferenceState, { kind: 'ready' }> = {
          kind: 'ready',
          ref,
          uid,
          sequence,
          items: snapshotCredentialReferences(payload.references),
        };
        commitReferenceState(ref, ready);
        return ready;
      } catch (error: unknown) {
        if (signal?.aborted || !isCurrent()) return null;
        commitReferenceState(ref, {
          kind: 'error',
          ref,
          uid,
          sequence,
          error: toAgmgrApiError(error),
        });
        return null;
      } finally {
        signal?.removeEventListener('abort', onAbort);
      }
    },
    [commitReferenceState, isCurrentReferenceSequence, nextReferenceSequence],
  );

  const toggleReferences = (credential: Credential) => {
    if (expandedRef === credential.ref) {
      setExpandedRef('');
      return;
    }
    setExpandedRef(credential.ref);
    const state = references[credential.ref];
    if (state === undefined || state.kind === 'error') void loadReferences(credential);
  };

  const openAdd = () => {
    setNotice('');
    setAddError(null);
    setAddSubmitted(false);
    setAddOpen(true);
  };

  const closeAdd = () => {
    if (saving) return;
    setAddOpen(false);
    setAddName('');
    setAddProvider('');
    setAddSecret('');
    setAddSubmitted(false);
    setAddError(null);
  };

  const submitAdd = () => {
    setAddSubmitted(true);
    setAddError(null);
    if (nameProblem || addSecretProblem !== null || saving || credentialLifecycle.locked) return;
    setSaving(true);
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
            ...(addProvider.trim() === '' ? {} : { provider: addProvider.trim() }),
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
        setAddOpen(false);
        setAddName('');
        setAddProvider('');
        setAddSecret('');
        setAddSubmitted(false);
        setNotice(t('settings.credentials.added', { name: created.name ?? addName.trim() }));
      },
      (error: unknown) => {
        setSaving(false);
        setAddSecret('');
        setAddError(toAgmgrApiError(error));
      },
    );
  };

  const openRotate = (credential: Credential) => {
    setRotateTarget(credential);
    setRotateSecret('');
    setRotateSubmitted(false);
    setRotateError(null);
    setNotice('');
    void loadReferences(credential);
  };

  const closeRotate = () => {
    if (rotating) return;
    setRotateTarget(null);
    setRotateSecret('');
    setRotateSubmitted(false);
    setRotateError(null);
  };

  const submitRotate = () => {
    if (rotateTarget === null || rotating || credentialLifecycle.locked) return;
    setRotateSubmitted(true);
    setRotateError(null);
    if (rotateSecretProblem !== null) return;
    const uid = credentialUid(rotateTarget.ref);
    if (uid === '') return;
    setRotating(true);
    const route = operationRequestForEndpoint(ENDPOINTS.credentialRotate, { uid });
    const attempt = credentialLifecycle.start<Credential>({
      kind: 'apply',
      scope: 'credentials',
      label: 'operation.action.apply',
      readback: { kind: 'credentials', uid },
      ...route,
      dispatch: (operation) =>
        rotateCredential(uid, { cred: rotateSecret }, operation === undefined ? {} : { operation }),
      summarize: () => ({ outcome: 'ok' }),
    });
    void attempt.promise.then(
      (updated) => {
        setRotating(false);
        setRotateTarget(null);
        setRotateSecret('');
        setRotateSubmitted(false);
        setNotice(t('credentials.rotate.done', { name: updated.name ?? '' }));
      },
      (error: unknown) => {
        setRotating(false);
        setRotateSecret('');
        setRotateError(toAgmgrApiError(error));
      },
    );
  };

  const openRevoke = (credential: Credential) => {
    const uid = credentialUid(credential.ref);
    if (uid === '') return;
    const target: CredentialRevokeTarget = Object.freeze({
      credential: Object.freeze({ ...credential }),
      ref: credential.ref,
      uid,
    });
    setRevokeTarget(target);
    setRevokeError(null);
    setRevokeChecking(false);
    setNotice('');
    void loadReferences(target.credential);
  };

  const closeRevoke = () => {
    if (revoking) return;
    setRevokeTarget(null);
    setRevokeError(null);
    setRevokeChecking(false);
  };

  const submitRevoke = (ready: Extract<ReferenceState, { kind: 'ready' }>) => {
    if (revokeTarget === null || revoking || credentialLifecycle.locked) return;
    const target = revokeTarget;
    const uid = target.uid;
    setRevoking(true);
    setRevokeChecking(true);
    setRevokeError(null);
    const route = operationRequestForEndpoint(ENDPOINTS.credentialRevoke, { uid });
    const attempt = credentialLifecycle.start<CredentialRevokePayload>({
      kind: 'apply',
      scope: 'credentials',
      label: 'operation.action.apply',
      readback: { kind: 'credentials', uid },
      ...route,
      prepareBeforeDispatch: async (_operation, { signal }) => {
        const fresh = await loadReferences(target.credential, signal);
        if (fresh === null) throw new Error('Credential references could not be verified');
        if (!credentialReferencesMatch(ready.items, fresh.items)) {
          commitReferenceState(target.ref, { ...fresh, changed: true });
          setRevokeChecking(false);
          throw new Error('Credential references changed; confirm the updated list');
        }
        setRevokeChecking(false);
      },
      dispatch: (operation) => revokeCredential(uid, operation === undefined ? {} : { operation }),
      summarize: () => ({ outcome: 'ok' }),
    });
    void attempt.promise.then(
      (payload) => {
        setRevoking(false);
        setRevokeChecking(false);
        setNotice(
          t('credentials.revoke.doneDetail', {
            name: payload.credential.name ?? '',
            total: payload.references.length,
          }),
        );
        setRevokeTarget(null);
      },
      (error: unknown) => {
        setRevoking(false);
        setRevokeChecking(false);
        if (error instanceof TrackedOperationPreparationError) return;
        setRevokeError(toAgmgrApiError(error));
      },
    );
  };

  const referenceState = revokeTarget === null ? undefined : references[revokeTarget.ref];
  const referenceReady =
    referenceState?.kind === 'ready' &&
    referenceState.ref === revokeTarget?.ref &&
    referenceState.uid === revokeTarget?.uid
      ? referenceState
      : null;
  const referenceItems = referenceReady?.items ?? [];
  const referenceCount = referenceItems.length;
  const rotateReferences = rotateTarget === null ? undefined : references[rotateTarget.ref];

  return (
    <>
      <Section
        title={t('settings.credentials.title')}
        actions={
          <Box sx={{ display: 'flex', alignItems: 'center', gap: spacing.x1 }}>
            <Button type="button" size="small" variant="text" onClick={credentials.reload}>
              {t('common.refresh')}
            </Button>
            <Button
              type="button"
              size="small"
              variant="contained"
              disabled={credentialLifecycle.locked}
              onClick={openAdd}
            >
              {t('tool.config.dialog.credential.add')}
            </Button>
            <ContextHelp label={t('settings.credentials.helpLabel')}>
              <Typography component="p" variant="body2" sx={{ m: 0 }}>
                {t('settings.credentials.help')}
              </Typography>
            </ContextHelp>
          </Box>
        }
      >
        <CredentialOperationStatuses lifecycle={credentialLifecycle} />
        {credentials.stale ? <Callout tone="warning">{t('states.stale.note')}</Callout> : null}
        {notice === '' ? null : (
          <Typography
            component="p"
            role="status"
            variant="body2"
            sx={{ m: 0, color: 'success.main' }}
          >
            {notice}
          </Typography>
        )}
        {credentials.state.kind === 'loading' ? <LoadingBlock rows={3} /> : null}
        {credentials.error === undefined ? null : (
          <ErrorState
            error={credentials.error}
            context={t('settings.credentials.loadError')}
            retry={credentials.reload}
            compact
          />
        )}
        {credentials.state.kind === 'empty' ||
        (credentials.data !== undefined && items.length === 0) ? (
          <EmptyState
            compact
            title={t('settings.credentials.empty')}
            action={
              <Button
                type="button"
                size="small"
                variant="outlined"
                disabled={credentialLifecycle.locked}
                onClick={openAdd}
              >
                {t('tool.config.dialog.credential.add')}
              </Button>
            }
          />
        ) : null}
        {items.length > 0 ? (
          <Typography
            component="p"
            variant="caption"
            sx={{ display: { xs: 'block', md: 'none' }, m: 0, color: 'text.secondary' }}
          >
            {t('settings.credentials.tableScrollHint')}
          </Typography>
        ) : null}
        {items.length > 0 ? (
          <TableScroll minWidth={820}>
            <Box
              component="table"
              aria-label={t('settings.credentials.tableLabel')}
              sx={{
                width: '100%',
                borderCollapse: 'collapse',
                '& thead th[data-credential-column="actions"]': {
                  position: 'sticky',
                  right: 0,
                  zIndex: 3,
                  minWidth: 144,
                  backgroundColor: color.surfaceSubtle,
                  borderLeft: `1px solid ${color.borderDivider}`,
                },
                '& tbody tr[data-credential-row="true"] td[data-credential-column="actions"]': {
                  position: 'sticky',
                  right: 0,
                  zIndex: 1,
                  backgroundColor: color.surface,
                },
              }}
            >
              <Box component="thead">
                <Box component="tr">
                  {[
                    { key: 'name', label: t('settings.credentials.column.name') },
                    { key: 'provider', label: t('settings.credentials.column.provider') },
                    { key: 'mask', label: t('settings.credentials.column.mask') },
                    { key: 'updated', label: t('settings.credentials.column.updated') },
                    { key: 'status', label: t('settings.credentials.column.status') },
                    { key: 'references', label: t('settings.credentials.column.references') },
                    { key: 'actions', label: t('settings.credentials.column.actions') },
                  ].map((label) => (
                    <Box
                      key={label.key}
                      component="th"
                      data-credential-column={label.key}
                      scope="col"
                      sx={{
                        position: 'sticky',
                        top: 0,
                        textAlign: 'left',
                        px: spacing.x2,
                        py: spacing.x2,
                        backgroundColor: color.surfaceSubtle,
                        color: 'text.secondary',
                        fontSize: fontSize.label,
                        fontWeight: fontWeight.medium,
                        whiteSpace: 'nowrap',
                      }}
                    >
                      {label.label}
                    </Box>
                  ))}
                </Box>
              </Box>
              <Box component="tbody">
                {items.map((credential) => (
                  <CredentialRow
                    key={credential.ref}
                    credential={credential}
                    referenceState={references[credential.ref]}
                    expanded={expandedRef === credential.ref}
                    busy={saving || rotating || revoking || credentialLifecycle.locked}
                    onToggleReferences={toggleReferences}
                    onLoadReferences={loadReferences}
                    onRotate={openRotate}
                    onRevoke={openRevoke}
                  />
                ))}
              </Box>
            </Box>
          </TableScroll>
        ) : null}
      </Section>

      <Dialog
        open={addOpen}
        onClose={closeAdd}
        maxWidth="sm"
        fullWidth
        aria-labelledby="credential-add-title"
      >
        <DialogTitle id="credential-add-title">
          {t('tool.config.dialog.credential.addTitle')}
        </DialogTitle>
        <DialogContent
          sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x3, pt: spacing.x2 }}
        >
          <CredentialOperationStatuses lifecycle={credentialLifecycle} />
          <Typography component="p" variant="body2" sx={{ m: 0, color: 'text.secondary' }}>
            {t('tool.config.dialog.credential.addNote')}
          </Typography>
          <TextField
            autoFocus
            required
            label={t('tool.config.dialog.credential.nameLabel')}
            value={addName}
            disabled={credentialLifecycle.locked}
            onChange={(event) => setAddName(event.target.value)}
            error={addSubmitted && nameProblem}
            helperText={
              addSubmitted && nameProblem
                ? t('tool.config.dialog.credential.nameRequired')
                : undefined
            }
            slotProps={{ htmlInput: { maxLength: 100 } }}
          />
          <TextField
            label={t('tool.config.dialog.credential.providerLabel')}
            value={addProvider}
            disabled={credentialLifecycle.locked}
            onChange={(event) => setAddProvider(event.target.value)}
            slotProps={{ htmlInput: { maxLength: 64 } }}
          />
          <TextField
            required
            type="password"
            autoComplete="new-password"
            label={t('tool.config.dialog.credential.secretLabel')}
            value={addSecret}
            disabled={credentialLifecycle.locked}
            onChange={(event) => setAddSecret(event.target.value)}
            error={addSubmitted && addSecretProblem !== null}
            helperText={
              addSubmitted && addSecretProblem !== null
                ? t(
                    `tool.config.dialog.credential.secret${addSecretProblem === 'required' ? 'Required' : addSecretProblem === 'mask' ? 'Mask' : 'Control'}`,
                  )
                : undefined
            }
          />
          {addError === null ? null : (
            <ErrorState
              error={addError}
              context={t('tool.config.dialog.credential.addErrorContext')}
              compact
            />
          )}
        </DialogContent>
        <DialogActions sx={{ px: spacing.x4, pb: spacing.x3 }}>
          <Button type="button" variant="text" disabled={saving} onClick={closeAdd}>
            {t('common.cancel')}
          </Button>
          <Button
            type="button"
            variant="contained"
            disabled={saving || credentialLifecycle.locked}
            onClick={submitAdd}
          >
            {saving
              ? t('tool.config.dialog.credential.saving')
              : t('tool.config.dialog.credential.submit')}
          </Button>
        </DialogActions>
      </Dialog>

      <Dialog
        open={rotateTarget !== null}
        onClose={closeRotate}
        maxWidth="sm"
        fullWidth
        aria-labelledby="credential-rotate-title"
      >
        <DialogTitle id="credential-rotate-title">{t('credentials.rotate.title')}</DialogTitle>
        <DialogContent
          sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x3, pt: spacing.x2 }}
        >
          <CredentialOperationStatuses lifecycle={credentialLifecycle} />
          {rotateTarget === null ? null : (
            <Typography
              component="p"
              variant="body2"
              sx={{ m: 0, fontWeight: fontWeight.semibold }}
            >
              {rotateTarget.name ?? t('tool.config.dialog.credential.nameNone')}
            </Typography>
          )}
          <Typography component="p" variant="body2" sx={{ m: 0, color: 'text.secondary' }}>
            {t('credentials.rotate.note')}
          </Typography>
          {rotateTarget === null ? null : (
            <Box>
              <Typography component="p" variant="overline" sx={{ m: 0 }}>
                {t('credentials.revoke.referencesTitle', {
                  total:
                    rotateReferences?.kind === 'ready' &&
                    rotateReferences.uid === credentialUid(rotateTarget.ref)
                      ? rotateReferences.items.length
                      : '—',
                })}
              </Typography>
              <CredentialReferences
                credential={rotateTarget}
                state={rotateReferences}
                onLoad={loadReferences}
              />
            </Box>
          )}
          <TextField
            autoFocus
            required
            type="password"
            autoComplete="new-password"
            label={t('credentials.rotate.secretLabel')}
            value={rotateSecret}
            disabled={credentialLifecycle.locked}
            onChange={(event) => setRotateSecret(event.target.value)}
            error={rotateSubmitted && rotateSecretProblem !== null}
            helperText={
              rotateSubmitted && rotateSecretProblem !== null
                ? t(
                    `tool.config.dialog.credential.secret${rotateSecretProblem === 'required' ? 'Required' : rotateSecretProblem === 'mask' ? 'Mask' : 'Control'}`,
                  )
                : undefined
            }
          />
          {rotateError === null ? null : (
            <>
              <Typography component="p" variant="body2" sx={{ m: 0, color: 'warning.dark' }}>
                {t('credentials.rotate.failedNote')}
              </Typography>
              <ErrorState
                error={rotateError}
                context={t('credentials.rotate.errorContext')}
                compact
              />
            </>
          )}
        </DialogContent>
        <DialogActions sx={{ px: spacing.x4, pb: spacing.x3 }}>
          <Button type="button" variant="text" disabled={rotating} onClick={closeRotate}>
            {t('common.cancel')}
          </Button>
          <Button
            type="button"
            variant="contained"
            disabled={
              rotating ||
              credentialLifecycle.locked ||
              rotateTarget === null ||
              rotateReferences?.kind !== 'ready' ||
              rotateReferences.uid !== credentialUid(rotateTarget.ref)
            }
            onClick={submitRotate}
          >
            {rotating ? t('credentials.rotate.submitting') : t('credentials.rotate.submit')}
          </Button>
        </DialogActions>
      </Dialog>

      <Dialog
        open={revokeTarget !== null}
        onClose={closeRevoke}
        maxWidth="sm"
        fullWidth
        aria-labelledby="credential-revoke-title"
      >
        <DialogTitle id="credential-revoke-title">
          {t('credentials.revoke.title', {
            name: revokeTarget?.credential.name ?? t('tool.config.dialog.credential.nameNone'),
          })}
        </DialogTitle>
        <DialogContent sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x2 }}>
          <CredentialOperationStatuses lifecycle={credentialLifecycle} />
          <Typography component="p" variant="body2" sx={{ m: 0 }}>
            {t('credentials.revoke.intro')}
          </Typography>
          <Typography component="p" variant="caption" sx={{ m: 0, color: 'text.secondary' }}>
            {t('credentials.revoke.noSilentDelete')}
          </Typography>
          {referenceState?.kind === 'loading' ? (
            <LoadingBlock rows={2} labelKey="credentials.revoke.referencesLoading" />
          ) : null}
          {referenceState?.kind !== 'error' ? null : (
            <>
              <Typography component="p" variant="body2" sx={{ m: 0, color: 'error.main' }}>
                {t('credentials.revoke.referencesFailed')}
              </Typography>
              <ErrorState
                error={referenceState.error}
                context={t('credentials.revoke.referencesContext')}
                retry={() => {
                  if (revokeTarget !== null) void loadReferences(revokeTarget.credential);
                }}
                compact
              />
            </>
          )}
          {referenceReady === null ? null : referenceCount === 0 ? (
            <Typography component="p" variant="body2" sx={{ m: 0, color: 'text.secondary' }}>
              {t('credentials.revoke.referencesEmpty')}
            </Typography>
          ) : (
            <Box
              component="ul"
              sx={{
                m: 0,
                pl: spacing.x4,
                display: 'flex',
                flexDirection: 'column',
                gap: spacing.x1,
              }}
            >
              {referenceItems.map((reference, index) => (
                <Box
                  component="li"
                  key={`${reference.kind}-${reference.toolId ?? ''}-${reference.profileId ?? ''}-${index}`}
                >
                  <Typography component="span" variant="body2">
                    {reference.name || t('tool.config.dialog.credential.nameNone')}
                  </Typography>
                  <StatusChip
                    tone="neutral"
                    label={t(`credentials.refKind.${reference.kind}`, {
                      defaultValue: reference.kind,
                    })}
                  />
                  {reference.toolId === undefined ? null : (
                    <Typography component="span" variant="caption" sx={{ color: 'text.secondary' }}>
                      {reference.toolId}
                    </Typography>
                  )}
                  {reference.active ? (
                    <StatusChip tone="warning" label={t('credentials.refActive')} />
                  ) : null}
                </Box>
              ))}
            </Box>
          )}
          {referenceReady?.changed === true ? (
            <Callout tone="warning">{t('credentials.revoke.referencesChanged')}</Callout>
          ) : null}
          {revokeError === null ? null : (
            <ErrorState
              error={revokeError}
              context={t('credentials.revoke.errorContext')}
              compact
            />
          )}
        </DialogContent>
        <DialogActions sx={{ px: spacing.x4, pb: spacing.x3 }}>
          <Button type="button" variant="text" disabled={revoking} onClick={closeRevoke}>
            {t('common.cancel')}
          </Button>
          <Button
            type="button"
            variant="contained"
            color="error"
            disabled={revoking || credentialLifecycle.locked || referenceReady === null}
            onClick={referenceReady === null ? undefined : () => submitRevoke(referenceReady)}
          >
            {revokeChecking
              ? t('credentials.revoke.checking')
              : revoking
                ? t('credentials.revoke.revoking')
                : t('credentials.revoke.confirm')}
          </Button>
        </DialogActions>
      </Dialog>
    </>
  );
}

export default SettingsCredentials;
