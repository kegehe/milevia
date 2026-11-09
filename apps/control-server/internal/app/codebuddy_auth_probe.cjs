// Only initialize/getUserInfo: never authenticate or send a model prompt.
// stdout contains a status, never raw account responses or stderr.
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawn } = require('node:child_process');
const config = JSON.parse(process.argv[1]);
if (process.argv[2]) config.binary = process.argv[2];
let child, timer, directory, finished = false, buffer = '', bytes = 0;
let initialized = false, credentialFailure = false;
// Local probes stay in Go's process group so cancellation reaps descendants.
// Remote probes own a group and clean it on completion, timeout or SIGHUP.
const ownChildGroup = process.platform !== 'win32' && !!process.argv[2];

function finish(authStatus, reason) {
  if (finished) return;
  finished = true;
  clearTimeout(timer);
  if (child?.pid) {
    if (process.platform === 'win32') {
      // Go's probe Job contains the helper and all descendants. Closing it after
      // the status response reaps the tree without launching slow taskkill.exe.
      try { child.kill('SIGKILL'); } catch {}
    } else {
      try { process.kill(ownChildGroup ? -child.pid : child.pid, 'SIGKILL'); } catch {}
    }
  }
  if (directory) { try { fs.rmSync(directory, { recursive: true, force: true }); } catch {} }
  fs.writeSync(1, JSON.stringify({ authStatus, ...(reason ? { reason } : {}) }));
  process.exit(0);
}
const fail = reason => finish('unreadable', reason);
process.on('SIGTERM', () => fail('cancelled'));
process.on('SIGINT', () => fail('cancelled'));
if (process.platform !== 'win32') process.on('SIGHUP', () => fail('cancelled'));
process.on('uncaughtException', () => fail('protocol'));
process.on('unhandledRejection', () => fail('protocol'));

function credentialSource() {
  let configured = !!(process.env.CODEBUDDY_API_KEY || process.env.CODEBUDDY_AUTH_TOKEN);
  const home = process.env.CODEBUDDY_CONFIG_DIR || path.join(os.homedir(), '.codebuddy');
  for (const name of ['settings.json', 'settings.local.json']) {
    let settings;
    try { settings = JSON.parse(fs.readFileSync(path.join(home, name), 'utf8').replace(/^\uFEFF/, '')); }
    catch (e) { if (e.code === 'ENOENT') continue; throw e; }
    configured ||= !!(settings.apiKeyHelper || settings.env?.CODEBUDDY_API_KEY || settings.env?.CODEBUDDY_AUTH_TOKEN);
  }
  return configured;
}

// Empty userInfo can mean credential restoration failed. Keep unknown when
// non-logged-out credential files remain. Inspect names only, never their secrets.
function hasStoredCredentials() {
  const root = process.platform === 'win32' ? path.join(os.homedir(), 'AppData', 'Local')
    : process.platform === 'darwin' ? path.join(os.homedir(), 'Library', 'Application Support')
    : path.join(os.homedir(), '.local', 'share');
  const auth = path.join(root, 'CodeBuddyExtension', 'Data', 'Public', 'auth');
  let names;
  try { names = fs.readdirSync(auth); } catch (e) { if (e.code === 'ENOENT') return false; throw e; }
  return names.some(name => name.endsWith('.info') && !names.includes(name + '.logged-out'));
}

function send(id, method, params) {
  child.stdin.write(JSON.stringify({ jsonrpc: '2.0', id, method, params }) + '\n');
}
function receive(message) {
  if (!message || message.jsonrpc !== '2.0') return fail('protocol');
  if (message.method) {
    // Deny requests: a status probe must not become a client/tool host.
    if (message.id !== undefined) child.stdin.write(JSON.stringify({ jsonrpc: '2.0', id: message.id, error: { code: -32601, message: 'Unsupported during status probe' } }) + '\n');
    return;
  }
  if (message.id !== (initialized ? 2 : 1)) return fail('protocol');
  if (message.error) return fail(message.error.code === -32601 ? 'unsupported' : 'protocol');
  if (!message.result || typeof message.result !== 'object' || Array.isArray(message.result)) return fail('protocol');
  if (!initialized) {
    if (message.result.protocolVersion !== 1) return fail('unsupported');
    initialized = true;
    send(2, '_codebuddy.ai/getUserInfo', {});
    return;
  }
  const info = message.result.userInfo;
  let configured;
  try { configured = credentialSource(); } catch { return fail('credentials_unreadable'); }
  if (info === undefined || info === null) {
    if (info === null || Object.keys(message.result).length) return fail('protocol');
    if (configured) return finish('configured');
    try { if (credentialFailure || hasStoredCredentials()) return fail('credentials_unreadable'); }
    catch { return fail('credentials_unreadable'); }
    return finish('logged_out');
  }
  if (typeof info !== 'object' || Array.isArray(info) || typeof info.userId !== 'string' || !info.userId.trim()) return fail('protocol');
  if (credentialFailure) return fail('credentials_unreadable');
  if (configured || info.userId.startsWith('anonymous_')) return finish('configured');
  if (typeof info.accessToken !== 'string' && typeof info.token !== 'string') return fail('protocol');
  if (!(info.accessToken || info.token)) return fail('credentials_unreadable');
  finish('logged_in');
}

try {
  let binary = config.binary, useNode = /\.[cm]?js$/i.test(binary);
  // npm's Windows shim needs cmd.exe; resolve its declared Node entry instead
  // of interpreting a shell script or interpolating shell arguments.
  if (/\.(cmd|bat)$/i.test(binary)) {
    const root = path.join(path.dirname(binary), 'node_modules', ...config.npmPackage.split('/'));
    const pkg = JSON.parse(fs.readFileSync(path.join(root, 'package.json'), 'utf8'));
    const bins = typeof pkg.bin === 'string' ? [pkg.bin] : Object.values(pkg.bin || {});
    if (pkg.name !== config.npmPackage || !bins.some(value => value.replace(/^\.\//, '') === config.binPath)) throw new Error('entry');
    binary = path.join(root, config.binPath);
    useNode = true;
  }
  directory = config.directory || fs.mkdtempSync(path.join(os.tmpdir(), 'milevia-auth-acp-'));
  const args = ['--acp', '--strict-mcp-config', '--mcp-config', '{"mcpServers":{}}', '--setting-sources', 'user', '--no-session-persistence'];
  child = spawn(useNode ? process.execPath : binary, useNode ? [binary, ...args] : args, {
    cwd: directory, windowsHide: true, detached: ownChildGroup,
    env: { ...process.env, DISABLE_AUTOUPDATER: '1', CODEBUDDY_SESSION_HISTORY_CLEANUP: '0' },
    stdio: ['pipe', 'pipe', 'pipe'],
  });
  timer = setTimeout(() => fail('timeout'), config.timeoutMs);
  child.on('error', () => fail('start_failed'));
  child.stdin.on('error', () => fail('unexpected_exit'));
  child.on('close', () => fail('unexpected_exit'));
  let stderrTail = '';
  child.stderr.on('data', data => {
    bytes += data.length;
    stderrTail = (stderrTail + data.toString('utf8')).slice(-4096);
    if (/restore auth error|failed to initialize authentication|credential.{0,80}(?:fail|unavailable|decrypt)|EACCES|EPERM/i.test(stderrTail)) credentialFailure = true;
    if (bytes > 1024 * 1024) fail('output_limit');
  });
  child.stdout.setEncoding('utf8');
  child.stdout.on('data', data => {
    bytes += Buffer.byteLength(data);
    buffer += data;
    if (bytes > 1024 * 1024 || buffer.length > 256 * 1024) return fail('output_limit');
    let end;
    while ((end = buffer.indexOf('\n')) >= 0) {
      const line = buffer.slice(0, end).trim();
      buffer = buffer.slice(end + 1);
      if (!line) continue;
      try { receive(JSON.parse(line)); } catch { return fail('protocol'); }
    }
  });
  send(1, 'initialize', { protocolVersion: 1, clientCapabilities: {}, clientInfo: { name: 'milevia-auth-probe', version: '1' } });
} catch { fail('start_failed'); }
