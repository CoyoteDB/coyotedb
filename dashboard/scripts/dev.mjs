// `npm run dev`: the mock API on 4331 and Vite (hot reload) on 4330, which proxies /v1 to the mock.
// Open the "Sign in" link the mock prints. Pass a scenario: npm run dev -- --scenario failover
import { spawn } from 'node:child_process';
import { join } from 'node:path';

const root = join(import.meta.dirname, '..');
const extra = process.argv.slice(2);
const mock = spawn(process.execPath, ['mock/server.mjs', '--port', '4331', '--ui-port', '4330', '--no-static', ...extra], { cwd: root, stdio: 'inherit' });
const vite = spawn(process.execPath, [join(root, 'node_modules/vite/bin/vite.js'), '--port', '4330', '--strictPort', '--host', '127.0.0.1'], { cwd: root, stdio: 'inherit' });
const stop = () => {
  mock.kill();
  vite.kill();
};
process.on('SIGINT', stop);
process.on('SIGTERM', stop);
mock.on('exit', stop);
vite.on('exit', stop);
