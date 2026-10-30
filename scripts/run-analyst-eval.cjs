// Loads local .env into the child only; never prints credentials or overwrites shell values.
const fs = require('node:fs');
const { spawnSync } = require('node:child_process');
const env = { ...process.env };
for (const line of fs.readFileSync('.env', 'utf8').split(/\r?\n/)) {
 const match = line.match(/^\s*([A-Za-z_][A-Za-z_0-9]*)\s*=\s*(.*)$/);
 if (!match || Object.hasOwn(env, match[1])) continue;
 let value = match[2].trim();
 if ((value.startsWith("'") && value.endsWith("'")) || (value.startsWith('"') && value.endsWith('"'))) value = value.slice(1, -1);
 env[match[1]] = value;
}
const go = process.env.GO_BINARY || (process.platform === 'win32' ? 'C:\\Program Files\\Go\\bin\\go.exe' : 'go');
const result = spawnSync(go, ['run', '.', 'analyst-eval', ...process.argv.slice(2)], { env, stdio: 'inherit', windowsHide: true });
if (result.error) { process.stderr.write('Unable to launch Go: ' + result.error.message + '\n'); process.exit(1); }
process.exit(result.status ?? 1);
