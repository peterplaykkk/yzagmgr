import { describe, expect, it } from 'vitest';
import {
  classifyContentLengthBand,
  classifyNativeProtocolRequest,
  createNativeProtocolTrafficCounters,
  recordNativeProtocolRequest,
  requestBodyChunkWithinLimit,
} from './native-protocol-guards';

describe('native CLI protocol peer guards', () => {
  const claudeMessagePath = '/private/v1/messages';
  const claudeCountTokensPath = '/private/v1/messages/count_tokens';
  const allowedPaths = new Set([claudeMessagePath, claudeCountTokensPath]);
  const betaPaths = new Set(allowedPaths);

  it('accepts only the exact Claude beta query on declared Anthropic endpoints', () => {
    const inference = classifyNativeProtocolRequest(
      'POST',
      `${claudeMessagePath}?beta=true`,
      allowedPaths,
      betaPaths,
      '1024',
    );
    const countTokens = classifyNativeProtocolRequest(
      'POST',
      `${claudeCountTokensPath}?beta=true`,
      allowedPaths,
      betaPaths,
      '128',
    );

    expect(inference).toMatchObject({ kind: 'accepted', protocolPath: 'anthropic_messages' });
    expect(countTokens).toMatchObject({ kind: 'accepted', protocolPath: 'anthropic_count_tokens' });
    expect(inference.facts).toMatchObject({ betaQueryPresent: true, queryKind: 'beta_true_only' });
  });

  it('rejects other query values, duplicate parameters, unsupported paths, and methods', () => {
    const rejectedTargets = [
      ['POST', `${claudeMessagePath}?beta=false`],
      ['POST', `${claudeMessagePath}?beta=true&beta=true`],
      ['POST', `${claudeMessagePath}?token=private`],
      ['GET', `${claudeMessagePath}?beta=true`],
      ['POST', '/private/v1/messages/stream?beta=true'],
      ['POST', 'http://[malformed'],
    ] as const;

    const decisions = rejectedTargets.map(([method, target]) =>
      classifyNativeProtocolRequest(method, target, allowedPaths, betaPaths),
    );

    expect(decisions.map((decision) => decision.kind)).toEqual(
      rejectedTargets.map(() => 'rejected'),
    );
    expect(decisions[0]).toMatchObject({ kind: 'rejected', reason: 'query' });
    expect(decisions[1]).toMatchObject({ kind: 'rejected', reason: 'query' });
    expect(decisions[2]).toMatchObject({ kind: 'rejected', reason: 'query' });
    expect(decisions[3]).toMatchObject({ kind: 'rejected', reason: 'method_or_path' });
    expect(decisions[4]).toMatchObject({ kind: 'rejected', reason: 'method_or_path' });
    expect(decisions[5]).toMatchObject({
      kind: 'rejected',
      reason: 'method_or_path',
      facts: { pathnameSuffix: 'other' },
    });
  });

  it('keeps count_tokens preflight distinguishable from the inference message', () => {
    const preflight = classifyNativeProtocolRequest(
      'POST',
      `${claudeCountTokensPath}?beta=true`,
      allowedPaths,
      betaPaths,
    );
    const inference = classifyNativeProtocolRequest(
      'POST',
      `${claudeMessagePath}?beta=true`,
      allowedPaths,
      betaPaths,
    );
    const ordinaryMessage = classifyNativeProtocolRequest(
      'POST',
      claudeMessagePath,
      allowedPaths,
      betaPaths,
    );
    const nonPostMessage = classifyNativeProtocolRequest(
      'GET',
      claudeMessagePath,
      allowedPaths,
      betaPaths,
    );
    const traffic = createNativeProtocolTrafficCounters();

    if (preflight.kind === 'accepted') recordNativeProtocolRequest(traffic, preflight.facts);
    if (inference.kind === 'accepted') recordNativeProtocolRequest(traffic, inference.facts);
    recordNativeProtocolRequest(traffic, ordinaryMessage.facts);
    recordNativeProtocolRequest(traffic, nonPostMessage.facts);

    expect(preflight).toMatchObject({ kind: 'accepted', protocolPath: 'anthropic_count_tokens' });
    expect(inference).toMatchObject({ kind: 'accepted', protocolPath: 'anthropic_messages' });
    expect(ordinaryMessage).toMatchObject({
      kind: 'accepted',
      facts: { betaQueryPresent: false, queryKind: 'none' },
    });
    expect(nonPostMessage).toMatchObject({
      kind: 'rejected',
      reason: 'method_or_path',
      facts: { methodIsPost: false },
    });
    expect(traffic).toMatchObject({
      total: 4,
      post: 3,
      otherMethod: 1,
      pathnameSuffix: { anthropic_count_tokens: 1, anthropic_messages: 3 },
      betaQueryPresent: 2,
      queryKind: { none: 2, beta_true_only: 2 },
    });
  });

  it('classifies exact OpenAI response and chat-completion paths', () => {
    const allowedOpenAiPaths = new Set(['/openai/v1/responses', '/v1/chat/completions']);
    const responses = classifyNativeProtocolRequest(
      'POST',
      '/openai/v1/responses',
      allowedOpenAiPaths,
    );
    const chatCompletions = classifyNativeProtocolRequest(
      'POST',
      '/v1/chat/completions',
      allowedOpenAiPaths,
    );
    const inventedBetaQuery = classifyNativeProtocolRequest(
      'POST',
      '/openai/v1/responses?beta=true',
      allowedOpenAiPaths,
    );

    expect(responses).toMatchObject({
      kind: 'accepted',
      protocolPath: 'openai_responses',
    });
    expect(chatCompletions).toMatchObject({
      kind: 'accepted',
      protocolPath: 'openai_chat_completions',
    });
    expect(inventedBetaQuery).toMatchObject({
      kind: 'rejected',
      reason: 'query',
    });
  });

  it('bounds declared and streamed bodies at 2 MiB while accepting the exact limit', () => {
    const twoMiB = 2 * 1024 * 1024;

    expect(classifyContentLengthBand(String(twoMiB))).toBe('256k_to_2m');
    expect(classifyContentLengthBand(String(twoMiB + 1))).toBe('over_2m');
    expect(classifyContentLengthBand('9007199254740992')).toBe('unknown');
    expect(classifyContentLengthBand('65535')).toBe('under_64k');
    expect(classifyContentLengthBand('65536')).toBe('64k_to_256k');
    expect(classifyContentLengthBand('262144')).toBe('256k_to_2m');
    expect(classifyContentLengthBand(null)).toBe('unknown');
    expect(classifyContentLengthBand('')).toBe('unknown');
    expect(classifyContentLengthBand('unknown')).toBe('unknown');
    expect(requestBodyChunkWithinLimit(twoMiB - 1, 1, twoMiB)).toBe(true);
    expect(requestBodyChunkWithinLimit(twoMiB, 1, twoMiB)).toBe(false);
    expect(requestBodyChunkWithinLimit(-1, 1, twoMiB)).toBe(false);
    expect(requestBodyChunkWithinLimit(twoMiB, -1, twoMiB)).toBe(false);
    expect(requestBodyChunkWithinLimit(0, 0, -1)).toBe(false);
    expect(requestBodyChunkWithinLimit(0, 0, 0)).toBe(true);
  });
});
