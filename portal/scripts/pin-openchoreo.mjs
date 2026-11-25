import { readFileSync, writeFileSync } from 'node:fs';
// The only editable OpenChoreo release pin; package.json resolutions are generated.
const version = readFileSync(
  new URL('../openchoreo-version', import.meta.url),
  'utf8',
).trim();
const path = new URL('../package.json', import.meta.url);
const pkg = JSON.parse(readFileSync(path, 'utf8'));
for (const name of Object.keys(pkg.resolutions))
  if (name.startsWith('@openchoreo/')) pkg.resolutions[name] = version;
writeFileSync(path, `${JSON.stringify(pkg, null, 2)}\n`);
