import { mkdtemp, mkdir, rm, utimes, writeFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { describe, expect, it } from 'vitest';
import { newestWebBuildInputMtime } from '../global-setup';

describe('web bundle freshness inputs', () => {
  it('ignores unit and E2E assets but detects bundle source and build metadata', async () => {
    const webDir = await mkdtemp(path.join(os.tmpdir(), 'yzagmgr-vite-inputs-'));
    try {
      const files = [
        'index.html',
        'public/favicon.svg',
        'src/main.tsx',
        'src/components/Widget.test.tsx',
        'src/components/Widget.spec.ts',
        'src/__tests__/Widget.tsx',
        'e2e/profile-draft.api.pw.ts',
        'e2e/tsconfig.json',
        'vite.config.ts',
        'tsconfig.json',
        'package.json',
        'package-lock.json',
      ];
      for (const relativePath of files) {
        const absolutePath = path.join(webDir, relativePath);
        await mkdir(path.dirname(absolutePath), { recursive: true });
        await writeFile(absolutePath, 'fixture');
      }

      const nowSeconds = Date.now() / 1000;
      const sourceSeconds = nowSeconds - 120;
      const distSeconds = sourceSeconds + 10;
      const testSeconds = sourceSeconds + 30;
      const setMtime = async (relativePath: string, seconds: number) => {
        const absolutePath = path.join(webDir, relativePath);
        await utimes(absolutePath, seconds, seconds);
      };
      for (const relativePath of [
        'index.html',
        'public/favicon.svg',
        'src/main.tsx',
        'vite.config.ts',
        'tsconfig.json',
        'package.json',
        'package-lock.json',
      ]) {
        await setMtime(relativePath, sourceSeconds);
      }
      await setMtime('src/components/Widget.test.tsx', testSeconds);
      await setMtime('src/components/Widget.spec.ts', testSeconds);
      await setMtime('src/__tests__/Widget.tsx', testSeconds);
      await setMtime('e2e/profile-draft.api.pw.ts', testSeconds);
      await setMtime('e2e/tsconfig.json', testSeconds);

      const afterTestOnlyEdits = await newestWebBuildInputMtime(webDir);
      expect(afterTestOnlyEdits).toBeLessThan(distSeconds * 1000);

      await setMtime('src/main.tsx', testSeconds + 10);
      expect(await newestWebBuildInputMtime(webDir)).toBeGreaterThan(distSeconds * 1000);

      await setMtime('src/main.tsx', sourceSeconds);
      await setMtime('tsconfig.json', testSeconds + 20);
      expect(await newestWebBuildInputMtime(webDir)).toBeGreaterThan(distSeconds * 1000);

      await setMtime('tsconfig.json', sourceSeconds);
      await setMtime('package-lock.json', testSeconds + 20);
      expect(await newestWebBuildInputMtime(webDir)).toBeGreaterThan(distSeconds * 1000);
    } finally {
      await rm(webDir, { recursive: true, force: true });
    }
  });
});
