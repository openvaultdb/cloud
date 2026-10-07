#!/usr/bin/env node
// Single-host staging recovery proof. Copy to the VM, run as root, and review before use.
import { randomBytes, createHash } from 'node:crypto';
import { spawn, spawnSync } from 'node:child_process';
import { chmodSync, closeSync, existsSync, lstatSync, mkdirSync, openSync, readFileSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { resolve } from 'node:path';

process.umask(0o077);
const ROOT = '/opt/datatug/registry-search';
const DATA = `${ROOT}/data`;
const SCRATCH = `${DATA}/registry-search-restore-proof`;
const SNAPSHOT = `${SCRATCH}/snapshot`;
const SCRATCH_MARKER = `${SCRATCH}/.pilot-snapshot-owner`;
const RESTORE = `${ROOT}/restore-proof`;
const BACKUPS = `${ROOT}/backups`;
const ENV = `${ROOT}/typesense.env`;
const PILOT = `${ROOT}/vm-pilot.mjs`;
const INSTALLED = `${ROOT}/recover.mjs`;
const RECEIPT = `${ROOT}/restore-proof-owner.json`;
const NAME = 'registry-search-typesense-restore';
const IMAGE = 'typesense/typesense:30.2@sha256:610f2d34b1f93d00762869da2c67736775e5798d19a2c8b91b014b8a0cc1e110';
const TASK = '01a114c1-e447-7a81-a869-0d82ca2a7747';
const MARKER = `${RESTORE}/.pilot-restore-owner`;
const OWNER = Object.freeze({ service: NAME, operator: 'alex', task: TASK, scratch: SCRATCH, snapshot: SNAPSHOT, restore: RESTORE, bind: '127.0.0.1:8109', cleanup: `node ${INSTALLED} cleanup`, note: 'Temporary snapshot and restore proof only; encrypted archive and passfile are retained.' });

function fail(message) { throw new Error(message); }
function privateFile(path) {
  const s = lstatSync(path);
  if (!s.isFile() || s.isSymbolicLink() || s.uid !== 0 || (s.mode & 0o077)) fail(`unsafe private file: ${path}`);
}
function ownedManifest(path) {
  const s = lstatSync(path);
  if (!s.isFile() || s.isSymbolicLink() || s.uid !== 0) fail(`unsafe manifest: ${path}`);
}
function privateDir(path) {
  const s = lstatSync(path);
  if (!s.isDirectory() || s.isSymbolicLink() || s.uid !== 0 || (s.mode & 0o077)) fail(`unsafe private directory: ${path}`);
}
function exclusiveFile(path, content) {
  const fd = openSync(path, 'wx', 0o600);
  try { writeFileSync(fd, content); } finally { closeSync(fd); }
  chmodSync(path, 0o600);
}
function command(binary, args, { allowMissing = false, timeout = 120_000 } = {}) {
  const result = spawnSync(binary, args, { encoding: 'utf8', timeout, maxBuffer: 1024 * 1024, env: { ...process.env, DOCKER_HOST: 'unix:///var/run/docker.sock' } });
  if (result.error) fail(`${binary} unavailable: ${result.error.code || result.error.message}`);
  if (result.status !== 0) {
    if (allowMissing && result.status === 1 && /No such (object|container)/i.test(result.stderr)) return null;
    fail(`${binary} ${args[0]} failed (exit ${result.status})`);
  }
  return result.stdout.trim();
}
function docker(args, options) { return command('docker', args, options); }
function inspect(name) {
  const raw = docker(['container', 'inspect', name], { allowMissing: true });
  return raw === null ? null : JSON.parse(raw)[0];
}
function key() {
  privateFile(ENV);
  const match = /^TYPESENSE_API_KEY=([A-Za-z0-9_-]{64})\nTYPESENSE_DATA_DIR=\/data\n$/.exec(readFileSync(ENV, 'utf8'));
  if (!match) fail('invalid pilot credential file');
  return match[1];
}
function preflight() {
  privateDir(ROOT); privateDir(DATA); privateFile(PILOT); privateFile(`${ROOT}/owner.json`);
  command('node', [PILOT, 'status'], { timeout: 45_000 });
  const primary = inspect('registry-search-typesense');
  if (!primary?.State?.Running || primary.Config?.Image !== IMAGE || primary.HostConfig?.NetworkMode !== 'bridge') fail('private primary pilot is not running');
  if (existsSync(SCRATCH)) fail('marked snapshot scratch path already exists; run the recorded cleanup command first');
  if (existsSync(RESTORE) || inspect(NAME)) fail('restore proof already exists; run the recorded cleanup command first');
  if (!existsSync(BACKUPS)) mkdirSync(BACKUPS, { mode: 0o700 });
  privateDir(BACKUPS);
}
function installSelfAndReceipt() {
  const source = readFileSync(new URL(import.meta.url));
  if (!existsSync(INSTALLED)) exclusiveFile(INSTALLED, source);
  privateFile(INSTALLED);
  if (!readFileSync(INSTALLED).equals(source)) fail('installed recovery helper differs from this version');
  if (!existsSync(RECEIPT)) exclusiveFile(RECEIPT, `${JSON.stringify(OWNER, null, 2)}\n`);
  privateFile(RECEIPT);
  let stored;
  try { stored = JSON.parse(readFileSync(RECEIPT, 'utf8')); } catch { fail('invalid restore owner receipt'); }
  for (const [name, value] of Object.entries(OWNER)) if (stored[name] !== value) fail(`restore owner receipt differs: ${name}`);
}
async function api(port, keyValue, method, path, timeout = 5000) {
  const response = await fetch(`http://127.0.0.1:${port}${path}`, { method, headers: { 'x-typesense-api-key': keyValue }, signal: AbortSignal.timeout(timeout) });
  if (!response.ok) fail(`Typesense ${method} ${path.split('?')[0]} on ${port} returned ${response.status}`);
  return response.json();
}
async function ready(port, keyValue) {
  for (let n = 0; n < 30; n++) {
    try {
      const health = await fetch(`http://127.0.0.1:${port}/health`, { signal: AbortSignal.timeout(1500) });
      if (health.ok && (await health.json()).ok === true) {
        const debug = await api(port, keyValue, 'GET', '/debug', 1500);
        if (debug.version === '30.2') return;
      }
    } catch { /* bounded wait while Typesense starts */ }
    await new Promise(resolveWait => setTimeout(resolveWait, 1000));
  }
  fail(`Typesense restore on ${port} did not become healthy in 30 seconds`);
}
function smokeQueries(manifestPath) {
  ownedManifest(manifestPath);
  const manifest = JSON.parse(readFileSync(manifestPath, 'utf8'));
  const queries = manifest.smoke_queries;
  const domains = ['meaninggraph', 'modelspec', 'ovdb'];
  if (!Array.isArray(queries) || domains.some(domain => !queries.some(q => q?.domain === domain && typeof q.q === 'string' && q.q && typeof q.id === 'string' && q.id))) fail('manifest needs a pinned smoke query for every domain');
  return queries;
}
async function state(port, keyValue, queries) {
  const alias = await api(port, keyValue, 'GET', '/aliases/registry_metadata');
  if (!/^registry_metadata_[a-f0-9]+$/.test(alias.collection_name ?? '')) fail('unexpected registry alias target');
  const collection = await api(port, keyValue, 'GET', `/collections/${alias.collection_name}`);
  if (collection.num_documents !== 1274) fail(`pilot corpus count differs from expected 1274 on ${port}`);
  const results = [];
  for (const query of queries) {
    const args = new URLSearchParams({ q: query.q, query_by: 'identifier,qualified_name,title,aliases,description', filter_by: `domain:=${query.domain} && visibility:=public`, per_page: '20' });
    const result = await api(port, keyValue, 'GET', `/collections/registry_metadata/documents/search?${args}`);
    if (!result.hits?.some(hit => hit.document?.id === query.id)) fail(`smoke query failed for ${query.domain} on ${port}`);
    results.push({ domain: query.domain, id: query.id, found: result.found });
  }
  return { alias: alias.collection_name, count: collection.num_documents, queries: results };
}
async function streamCommands(producerBin, producerArgs, consumerBin, consumerArgs) {
  const producer = spawn(producerBin, producerArgs, { stdio: ['ignore', 'pipe', 'ignore'] });
  const consumer = spawn(consumerBin, consumerArgs, { stdio: ['pipe', 'ignore', 'ignore'] });
  producer.stdout.pipe(consumer.stdin);
  consumer.stdin.on('error', () => { /* child exit is reported by the close handler */ });
  const wait = child => new Promise((resolveWait, reject) => {
    child.on('error', reject);
    child.on('close', code => code === 0 ? resolveWait() : reject(new Error(`${child.spawnfile} exited ${code}`)));
  });
  const timeout = setTimeout(() => { producer.kill('SIGKILL'); consumer.kill('SIGKILL'); }, 60_000);
  try { await Promise.all([wait(producer), wait(consumer)]); }
  catch (error) { producer.kill(); consumer.kill(); throw error; }
  finally { clearTimeout(timeout); }
}
function ownedRestore(container) {
  return container?.Name === `/${NAME}` && container.Config?.Image === IMAGE &&
    container.Config?.Labels?.['dev.datatug.owner'] === 'alex' && container.Config?.Labels?.['dev.datatug.task'] === TASK &&
    container.Config?.Labels?.['dev.datatug.service'] === 'registry-search-restore-proof' &&
    container.Mounts?.some(mount => mount.Type === 'bind' && mount.Source === RESTORE && mount.Destination === '/data');
}
function verifyRestore(container) {
  if (!ownedRestore(container)) fail('restore container does not match owner receipt');
  const host = container.HostConfig || {};
  if (host.NetworkMode !== 'bridge' || host.Privileged !== false || (host.CapAdd?.length ?? 0) !== 0 ||
      host.NanoCpus !== 1_000_000_000 || host.Memory !== 1024 ** 3 || host.MemorySwap !== 1024 ** 3 ||
      JSON.stringify(host.PortBindings?.['8108/tcp']) !== JSON.stringify([{ HostIp: '127.0.0.1', HostPort: '8109' }]) ||
      Object.keys(host.PortBindings || {}).length !== 1) fail('restore container isolation or limits differ');
}
function cleanup() {
  privateDir(ROOT);
  privateFile(RECEIPT);
  const receipt = JSON.parse(readFileSync(RECEIPT, 'utf8'));
  for (const [name, value] of Object.entries(OWNER)) if (receipt[name] !== value) fail(`restore receipt differs: ${name}`);
  const container = inspect(NAME);
  if (container) {
    if (!ownedRestore(container)) fail('restore container is not owned by this proof');
    docker(['container', 'rm', '--force', NAME]);
  }
  if (existsSync(RESTORE)) {
    privateDir(RESTORE); privateFile(MARKER);
    if (readFileSync(MARKER, 'utf8') !== `${TASK}\n`) fail('restore directory marker differs');
    rmSync(RESTORE, { recursive: true, force: false });
  }
  if (existsSync(SCRATCH)) {
    privateDir(SCRATCH); privateFile(SCRATCH_MARKER);
    if (readFileSync(SCRATCH_MARKER, 'utf8') !== `${TASK}\n`) fail('snapshot scratch marker differs');
    rmSync(SCRATCH, { recursive: true, force: false });
  }
  console.log('restore proof container, directory, and temporary snapshot removed; encrypted backup and passfile retained');
}
async function prove(manifestPath) {
  preflight();
  const queries = smokeQueries(manifestPath);
  const keyValue = key();
  const before = await state(8108, keyValue, queries);
  installSelfAndReceipt();
  try {
  mkdirSync(SCRATCH, { mode: 0o700 }); privateDir(SCRATCH);
  exclusiveFile(SCRATCH_MARKER, `${TASK}\n`);
  const snapshot = await api(8108, keyValue, 'POST', '/operations/snapshot?snapshot_path=/data/registry-search-restore-proof/snapshot', 60_000);
  if (snapshot.success !== true) fail('Typesense did not confirm snapshot');
  const snapshotStat = lstatSync(SNAPSHOT);
  if (!snapshotStat.isDirectory() || snapshotStat.isSymbolicLink()) fail('snapshot destination is unsafe');
  const id = `${new Date().toISOString().replace(/[:.]/g, '-')}-${randomBytes(8).toString('hex')}`;
  const backupDir = `${BACKUPS}/${id}`;
  mkdirSync(backupDir, { mode: 0o700 }); privateDir(backupDir);
  const pass = `${backupDir}/archive.pass`;
  const archive = `${backupDir}/snapshot.tar.enc`;
  exclusiveFile(pass, `${randomBytes(48).toString('base64url')}\n`);
  await streamCommands('tar', ['-C', SCRATCH, '-cf', '-', 'snapshot'], 'openssl', ['enc', '-aes-256-cbc', '-pbkdf2', '-iter', '250000', '-salt', '-pass', `file:${pass}`, '-out', archive]);
  privateFile(archive);
  if (statSync(archive).size < 32) fail('encrypted archive is unexpectedly small');
  const digest = createHash('sha256').update(readFileSync(archive)).digest('hex');
  exclusiveFile(`${backupDir}/archive.sha256`, `${digest}  snapshot.tar.enc\n`);
  console.log(`encrypted backup: ${archive}; passfile: ${pass}; sha256: ${digest}`);

  mkdirSync(RESTORE, { mode: 0o700 }); privateDir(RESTORE);
  exclusiveFile(MARKER, `${TASK}\n`);
    await streamCommands('openssl', ['enc', '-d', '-aes-256-cbc', '-pbkdf2', '-iter', '250000', '-pass', `file:${pass}`, '-in', archive], 'tar', ['-C', RESTORE, '--strip-components=1', '-xf', '-']);
    docker(['container', 'run', '--detach', '--name', NAME,
      '--label', 'dev.datatug.owner=alex', '--label', `dev.datatug.task=${TASK}`, '--label', 'dev.datatug.service=registry-search-restore-proof',
      '--network', 'bridge', '--cpus', '1', '--memory', '1g', '--memory-swap', '1g', '--restart', 'no',
      '--log-driver', 'json-file', '--log-opt', 'max-size=10m', '--log-opt', 'max-file=2',
      '--publish', '127.0.0.1:8109:8108', '--mount', `type=bind,src=${RESTORE},dst=/data`, '--env-file', ENV, IMAGE]);
    verifyRestore(inspect(NAME));
    await ready(8109, keyValue);
    const after = await state(8109, keyValue, queries);
    if (JSON.stringify(after) !== JSON.stringify(before)) fail('restored alias, count, or smoke query results differ');
    console.log(`restore proof passed: alias ${after.alias}, ${after.count} documents, ${queries.length} smoke queries`);
  } finally {
    cleanup();
  }
}

async function main() {
  if (process.platform !== 'linux' || process.getuid?.() !== 0) fail('run as root on the Linux VM');
  const action = process.argv[2];
  if (action === 'cleanup' && process.argv.length === 3) { cleanup(); return; }
  if (action === 'prove' && process.argv.length === 4) { await prove(resolve(process.argv[3])); return; }
  fail('usage: node registry-search-vm-recover.mjs prove /path/to/pinned-manifest.json | cleanup');
}

if (process.argv[1] && resolve(process.argv[1]) === new URL(import.meta.url).pathname) {
  main().catch(error => { console.error(`recovery proof: ${error.message}`); process.exitCode = 1; });
}
