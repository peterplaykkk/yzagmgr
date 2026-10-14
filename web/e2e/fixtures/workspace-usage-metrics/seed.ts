import { execFileSync } from 'node:child_process';
import path from 'node:path';
import { REPO_ROOT, type IsolatedDaemon, type PolicySeed } from '../daemon';

/** Stable fixture identities returned by the private Go Store.Record seeder. */
export interface WorkspaceUsageMetricsSeed {
  schema: 1;
  baseUnix: number;
  uc110A12?: {
    fromUnix: number;
    toUnix: number;
    mappedFailure: ModelUsageSeedRow;
    emptyUpstream: ModelUsageSeedRow & { upstreamModel: '' };
    selfEchoFailure: ModelUsageSeedRow & { model: string };
  };
}

export interface UsageSeedRow {
  id: string;
  status: number;
  inputTokens: number;
  outputTokens: number;
  durationMs: number;
  durationState: 'measured' | 'unknown';
}

export interface ModelUsageSeedRow extends UsageSeedRow {
  requestedModel: string;
  upstreamModel: string;
}

interface RawUsageSeedRow extends UsageSeedRow {
  requestedModel: string;
  upstreamModel: string;
}

interface RawSelfEchoSeedRow extends RawUsageSeedRow {
  model: string;
}

interface RawWorkspaceUsageMetricsSeed {
  schema: number;
  baseUnix: number;
  uc110A12?: {
    fromUnix: number;
    toUnix: number;
    mappedFailure: RawUsageSeedRow;
    emptyUpstream: RawUsageSeedRow;
    selfEchoFailure: RawSelfEchoSeedRow;
  };
}

export async function seedWorkspaceUsageMetrics(
  daemon: IsolatedDaemon,
  options: { uc110A12?: boolean } = {},
): Promise<WorkspaceUsageMetricsSeed> {
  const policy = await daemon.api<PolicySeed>('GET', '/policy');
  await daemon.stop();

  const dbPath = path.join(daemon.home, '.yzagmgr', 'data', 'usage.db');
  const fixturePath = path.join(
    REPO_ROOT,
    'web',
    'e2e',
    'fixtures',
    'workspace-usage-metrics',
    'main.go',
  );
  const commandArgs = [fixturePath];
  if (options.uc110A12 === true) commandArgs.push('--uc110-a12');
  commandArgs.push(dbPath);

  let seed: WorkspaceUsageMetricsSeed | undefined;
  let seedError: unknown;
  let seedFailed = false;
  try {
    const output = execFileSync('go', ['run', ...commandArgs], {
      cwd: REPO_ROOT,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'pipe'],
      timeout: 120_000,
    });
    seed = parseSeedReport(output);
    if (options.uc110A12 === true && seed.uc110A12 === undefined) {
      throw new Error('usage fixture omitted the requested UC-110 A12 row tuple');
    }
  } catch (error) {
    seedFailed = true;
    seedError = error;
  }

  try {
    await daemon.restart(policy);
  } catch (restartError) {
    if (seedFailed) {
      throw new AggregateError(
        [seedError, restartError],
        'usage fixture seeding failed and the isolated daemon did not restart',
      );
    }
    throw restartError;
  }
  if (seedFailed) throw seedError;
  if (seed === undefined) throw new Error('usage fixture returned no seed report');
  return seed;
}

function parseSeedReport(output: string): WorkspaceUsageMetricsSeed {
  let value: unknown;
  try {
    value = JSON.parse(output.trim()) as unknown;
  } catch {
    throw new Error(`usage fixture did not return its JSON seed report: ${output}`);
  }
  if (typeof value !== 'object' || value === null) {
    throw new Error('usage fixture returned a non-object seed report');
  }
  const report = value as Partial<RawWorkspaceUsageMetricsSeed>;
  if (report.schema !== 1 || !Number.isSafeInteger(report.baseUnix)) {
    throw new Error('usage fixture returned an unsupported schema or invalid base timestamp');
  }
  if (report.uc110A12 === undefined) {
    return { schema: 1, baseUnix: report.baseUnix as number };
  }
  const tuple = report.uc110A12;
  if (
    !Number.isSafeInteger(tuple.fromUnix) ||
    !Number.isSafeInteger(tuple.toUnix) ||
    tuple.toUnix <= tuple.fromUnix
  ) {
    throw new Error('usage fixture returned an invalid UC-110 A12 window tuple');
  }
  const mappedFailure = normalizedRow(tuple.mappedFailure, 'mapped failure');
  const emptyUpstream = normalizedRow(tuple.emptyUpstream, 'empty upstream');
  const selfEchoFailure = normalizedRow(tuple.selfEchoFailure, 'self-echo failure');
  if (emptyUpstream.upstreamModel !== '') {
    throw new Error('usage fixture empty-upstream row did not have an empty upstream model');
  }
  if (
    tuple.selfEchoFailure.model !== tuple.selfEchoFailure.requestedModel ||
    tuple.selfEchoFailure.model !== tuple.selfEchoFailure.upstreamModel
  ) {
    throw new Error('usage fixture self-echo row did not return its requested model alias');
  }
  return {
    schema: 1,
    baseUnix: report.baseUnix as number,
    uc110A12: {
      fromUnix: tuple.fromUnix,
      toUnix: tuple.toUnix,
      mappedFailure,
      emptyUpstream: { ...emptyUpstream, upstreamModel: '' },
      selfEchoFailure: { ...selfEchoFailure, model: tuple.selfEchoFailure.model },
    },
  };
}

function normalizedRow(row: RawUsageSeedRow, label: string): ModelUsageSeedRow {
  if (
    typeof row !== 'object' ||
    row === null ||
    typeof row.id !== 'string' ||
    typeof row.requestedModel !== 'string' ||
    typeof row.upstreamModel !== 'string' ||
    !Number.isSafeInteger(row.status) ||
    !Number.isSafeInteger(row.inputTokens) ||
    !Number.isSafeInteger(row.outputTokens) ||
    !Number.isSafeInteger(row.durationMs) ||
    (row.durationState !== 'measured' && row.durationState !== 'unknown')
  ) {
    throw new Error(`usage fixture returned an invalid ${label} row tuple`);
  }
  return {
    id: row.id,
    requestedModel: row.requestedModel,
    upstreamModel: row.upstreamModel,
    status: row.status,
    inputTokens: row.inputTokens,
    outputTokens: row.outputTokens,
    durationMs: row.durationMs,
    durationState: row.durationState,
  };
}
