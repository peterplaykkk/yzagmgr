import { readdirSync, readFileSync } from 'node:fs';
import { join, relative, sep } from 'node:path';

/** Read the TypeScript source tree from disk without importing files as `?raw` modules. */
export function readTypeScriptTree(root: string, keyPrefix: '.' | '..'): Record<string, string> {
  const sources: Record<string, string> = {};

  const visit = (directory: string): void => {
    for (const entry of readdirSync(directory, { withFileTypes: true })) {
      if (entry.name.startsWith('.')) continue;
      const absolutePath = join(directory, entry.name);
      if (entry.isDirectory()) {
        if (entry.name !== 'node_modules') visit(absolutePath);
        continue;
      }
      if (!entry.isFile() || !/\.tsx?$/.test(entry.name)) continue;

      const sourcePath = relative(root, absolutePath).split(sep).join('/');
      sources[`${keyPrefix}/${sourcePath}`] = readFileSync(absolutePath, 'utf8');
    }
  };

  visit(root);
  return sources;
}
