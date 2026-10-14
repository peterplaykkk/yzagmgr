export type NativeProtocolPathnameSuffix =
  | 'anthropic_messages'
  | 'anthropic_count_tokens'
  | 'openai_responses'
  | 'openai_chat_completions'
  | 'other';

export type NativeProtocolContentLengthBand =
  'unknown' | 'under_64k' | '64k_to_256k' | '256k_to_2m' | 'over_2m';

export interface NativeProtocolRequestFacts {
  methodIsPost: boolean;
  pathnameSuffix: NativeProtocolPathnameSuffix;
  betaQueryPresent: boolean;
  queryKind: 'none' | 'beta_true_only' | 'other';
  contentLengthBand: NativeProtocolContentLengthBand;
}

export interface NativeProtocolTrafficCounters {
  total: number;
  post: number;
  otherMethod: number;
  pathnameSuffix: Record<NativeProtocolPathnameSuffix, number>;
  betaQueryPresent: number;
  queryKind: { none: number; beta_true_only: number; other: number };
  contentLengthBand: Record<NativeProtocolContentLengthBand, number>;
}

export function createNativeProtocolTrafficCounters(): NativeProtocolTrafficCounters {
  return {
    total: 0,
    post: 0,
    otherMethod: 0,
    pathnameSuffix: {
      anthropic_messages: 0,
      anthropic_count_tokens: 0,
      openai_responses: 0,
      openai_chat_completions: 0,
      other: 0,
    },
    betaQueryPresent: 0,
    queryKind: { none: 0, beta_true_only: 0, other: 0 },
    contentLengthBand: {
      unknown: 0,
      under_64k: 0,
      '64k_to_256k': 0,
      '256k_to_2m': 0,
      over_2m: 0,
    },
  };
}

export function recordNativeProtocolRequest(
  counters: NativeProtocolTrafficCounters,
  facts: NativeProtocolRequestFacts,
): void {
  counters.total += 1;
  if (facts.methodIsPost) counters.post += 1;
  else counters.otherMethod += 1;
  counters.pathnameSuffix[facts.pathnameSuffix] += 1;
  if (facts.betaQueryPresent) counters.betaQueryPresent += 1;
  counters.queryKind[facts.queryKind] += 1;
  counters.contentLengthBand[facts.contentLengthBand] += 1;
}

export type NativeProtocolPathDecision =
  | {
      kind: 'accepted';
      protocolPath: NativeProtocolPathnameSuffix;
      facts: NativeProtocolRequestFacts;
    }
  | {
      kind: 'rejected';
      reason: 'method_or_path' | 'query';
      facts: NativeProtocolRequestFacts;
    };

function pathnameSuffix(pathname: string): NativeProtocolPathnameSuffix {
  if (pathname.endsWith('/v1/messages/count_tokens')) return 'anthropic_count_tokens';
  if (pathname.endsWith('/v1/messages')) return 'anthropic_messages';
  if (pathname.endsWith('/openai/v1/responses')) return 'openai_responses';
  if (pathname.endsWith('/v1/chat/completions')) return 'openai_chat_completions';
  return 'other';
}

export function classifyContentLengthBand(
  value: string | null | undefined,
): NativeProtocolContentLengthBand {
  if (value === null || value === undefined || !/^\d+$/.test(value)) return 'unknown';
  const length = Number(value);
  if (!Number.isSafeInteger(length)) return 'unknown';
  if (length < 64 * 1024) return 'under_64k';
  if (length < 256 * 1024) return '64k_to_256k';
  if (length <= 2 * 1024 * 1024) return '256k_to_2m';
  return 'over_2m';
}

export function requestBodyChunkWithinLimit(
  currentBytes: number,
  nextChunkBytes: number,
  limitBytes: number,
): boolean {
  return (
    Number.isSafeInteger(currentBytes) &&
    Number.isSafeInteger(nextChunkBytes) &&
    Number.isSafeInteger(limitBytes) &&
    currentBytes >= 0 &&
    nextChunkBytes >= 0 &&
    limitBytes >= 0 &&
    currentBytes + nextChunkBytes <= limitBytes
  );
}

export function classifyNativeProtocolRequest(
  method: string,
  target: string,
  expectedPathnames: ReadonlySet<string>,
  betaQueryPathnames: ReadonlySet<string> = new Set(),
  contentLength: string | null | undefined = undefined,
): NativeProtocolPathDecision {
  let pathname = '';
  let search = '';
  try {
    const parsed = new URL(target, 'http://private-peer.invalid');
    pathname = parsed.pathname;
    search = parsed.search;
  } catch {
    // Keep malformed request targets as a fixed path rejection; never expose the raw value.
  }
  const betaQueryPresent = new URLSearchParams(search).get('beta') === 'true';
  const facts: NativeProtocolRequestFacts = {
    methodIsPost: method === 'POST',
    pathnameSuffix: pathnameSuffix(pathname),
    betaQueryPresent,
    queryKind: search === '' ? 'none' : search === '?beta=true' ? 'beta_true_only' : 'other',
    contentLengthBand: classifyContentLengthBand(contentLength),
  };
  if (!facts.methodIsPost || !expectedPathnames.has(pathname)) {
    return { kind: 'rejected', reason: 'method_or_path', facts };
  }
  if (search !== '' && !(betaQueryPathnames.has(pathname) && search === '?beta=true')) {
    return { kind: 'rejected', reason: 'query', facts };
  }
  return { kind: 'accepted', protocolPath: facts.pathnameSuffix, facts };
}
