// Load local .env into the child only. Never print credentials or replace shell values.
const fs = require('node:fs');
const { spawnSync } = require('node:child_process');
const [command, ...args] = process.argv.slice(2);
if (!['research-run', 'research-eval'].includes(command)) {
  process.stderr.write('Usage: node scripts/run-research.cjs research-run|research-eval [flags]\n');
  process.exit(1);
}
const env = { ...process.env };
if (fs.existsSync('.env')) {
  for (const line of fs.readFileSync('.env', 'utf8').split(/\r?\n/)) {
    const match = line.match(/^\s*([A-Za-z_][A-Za-z_0-9]*)\s*=\s*(.*)$/);
    if (!match || Object.hasOwn(env, match[1])) continue;
    let value = match[2].trim();
    if ((value.startsWith("'") && value.endsWith("'")) || (value.startsWith('"') && value.endsWith('"'))) value = value.slice(1, -1);
    env[match[1]] = value;
  }
}
const go = process.env.GO_BINARY || (process.platform === 'win32' ? 'C:\\Program Files\\Go\\bin\\go.exe' : 'go');
const result = spawnSync(go, ['run', '.', command, ...args], { env, stdio: 'inherit', windowsHide: true });
if (result.error) { process.stderr.write('Unable to launch Go: ' + result.error.message + '\n'); process.exit(1); }
process.exit(result.status ?? 1);
