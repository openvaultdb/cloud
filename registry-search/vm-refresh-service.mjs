#!/usr/bin/env node
// Root-owned scheduled publication on the existing VM pilot. No secrets are copied.
import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { existsSync, lstatSync, mkdirSync, readFileSync, writeFileSync, renameSync, rmSync, chmodSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = '/opt/datatug/registry-search';
const live = join(root, 'live');
const state = join(root, 'state');
const ownerFile = join(root, 'live-owner.json');
const serviceName = 'registry-search-refresh.service';
const timerName = 'registry-search-refresh.timer';
const unitDir = '/etc/systemd/system';
const files = ['refresh.mjs', 'engine-policy.mjs', 'merge.mjs', 'schema.mjs', 'publish.mjs', 'lock.mjs', 'lock_exec.py'];
const task = '01a114c1-e447-7a81-a869-0d82ca2a7747';

function fail(message) { throw new Error(message); }
function requireRootFile(path) {
  const stat = lstatSync(path);
  if (!stat.isFile() || stat.isSymbolicLink() || stat.uid !== 0 || (stat.mode & 0o077)) fail(`unsafe root file: ${path}`);
}
function requireRootDir(path) {
  const stat = lstatSync(path);
  if (!stat.isDirectory() || stat.isSymbolicLink() || stat.uid !== 0 || (stat.mode & 0o077)) fail(`unsafe root directory: ${path}`);
}
function systemctl(args) {
  const result = spawnSync('systemctl', args, { encoding: 'utf8', maxBuffer: 4096, timeout: 30_000 });
  if (result.error || result.status !== 0) fail(`systemctl ${args[0]} failed`);
  return result.stdout.trim();
}
function privateWrite(path, data) {
  const temp = `${path}.${process.pid}.tmp`;
  writeFileSync(temp, data, { mode: 0o600, flag: 'wx' });
  chmodSync(temp, 0o600);
  renameSync(temp, path);
}
function hash(bytes) { return createHash('sha256').update(bytes).digest('hex'); }
function sourceFiles() {
  const dir = dirname(fileURLToPath(import.meta.url));
  return Object.fromEntries(files.map(name => [name, readFileSync(join(dir, name))]));
}
function receipt() {
  if (!existsSync(ownerFile)) return null;
  requireRootFile(ownerFile);
  const data = JSON.parse(readFileSync(ownerFile, 'utf8'));
  if (data.owner !== 'alex' || data.task !== task || data.service !== serviceName || data.timer !== timerName || data.stop !== `node ${join(live, 'vm-refresh-service.mjs')} stop` || data.teardown !== `node ${join(live, 'vm-refresh-service.mjs')} teardown`) fail('refresh ownership mismatch');
  return data;
}
function verifyPilot() {
  requireRootDir(root);
  requireRootDir(state);
  requireRootFile(join(root, 'owner.json'));
  requireRootFile(join(root, 'typesense.env'));
  const pilot = JSON.parse(readFileSync(join(root, 'owner.json'), 'utf8'));
  if (pilot.operator !== 'alex' || pilot.task !== task || pilot.bind !== '127.0.0.1:8108') fail('unexpected VM pilot owner');
  if (!existsSync(join(state, 'publication.json'))) fail('missing existing publication state');
  requireRootFile(join(state, 'publication.json'));
}
export function serviceUnit(node) {
  return `[Unit]\nDescription=Registry search public corpus refresh (owner alex, task ${task})\nAfter=network-online.target docker.service\nWants=network-online.target\n\n[Service]\nType=oneshot\nUser=root\nGroup=root\nUMask=0077\nEnvironmentFile=${join(root, 'typesense.env')}\nEnvironment=REGISTRY_SEARCH_MODE=production\nEnvironment=REGISTRY_TYPESENSE_DEPLOYMENT=vm-pilot\nEnvironment=REGISTRY_TYPESENSE_ORIGIN=https://vm1.sneat.dev/\nEnvironment=REGISTRY_SEARCH_STATE_DIR=${state}\nExecStart=${node} ${join(live, 'refresh.mjs')}\nWorkingDirectory=${live}\nNoNewPrivileges=yes\nPrivateTmp=yes\nProtectHome=yes\nProtectSystem=strict\nReadWritePaths=${state}\nMemoryMax=768M\nCPUQuota=100%\nTimeoutStartSec=240s\nTimeoutStopSec=15s\nStandardOutput=journal\nStandardError=journal\n`;
}
const timer = `[Unit]\nDescription=Refresh registry search every five minutes (owner alex, task ${task})\n\n[Timer]\nOnBootSec=2min\nOnUnitActiveSec=5min\nRandomizedDelaySec=20s\nPersistent=true\nUnit=${serviceName}\n\n[Install]\nWantedBy=timers.target\n`;

function verifyInstalled(data) {
  requireRootDir(live);
  for (const [name, digest] of Object.entries(data.files)) {
    const path = join(live, name);
    requireRootFile(path);
    if (hash(readFileSync(path)) !== digest) fail(`installed source drift: ${name}`);
  }
  for (const name of [serviceName, timerName]) {
    const path = join(unitDir, name);
    requireRootFile(path);
    if (hash(readFileSync(path)) !== data.units[name]) fail(`installed unit drift: ${name}`);
  }
}

function main(command) {
  if (process.platform !== 'linux' || process.getuid?.() !== 0) fail('run as root on Linux');
  if (!['install', 'status', 'stop', 'teardown'].includes(command)) fail('usage: vm-refresh-service.mjs install|status|stop|teardown');
  verifyPilot();
  const current = receipt();
  if (command === 'install') {
    const sources = sourceFiles();
    const self = readFileSync(new URL(import.meta.url));
    sources['vm-refresh-service.mjs'] = self;
    const node = process.execPath;
    const units = { [serviceName]: serviceUnit(node), [timerName]: timer };
    const data = { owner: 'alex', task, service: serviceName, timer: timerName, stop: `node ${join(live, 'vm-refresh-service.mjs')} stop`, teardown: `node ${join(live, 'vm-refresh-service.mjs')} teardown`, files: Object.fromEntries(Object.entries(sources).map(([name, bytes]) => [name, hash(bytes)])), units: Object.fromEntries(Object.entries(units).map(([name, text]) => [name, hash(text)])) };
    if (current) {
      verifyInstalled(current);
      if (JSON.stringify(current) !== JSON.stringify(data)) fail('installed version differs; stop and teardown before installing a new version');
    } else {
      if (existsSync(live) || existsSync(join(unitDir, serviceName)) || existsSync(join(unitDir, timerName))) fail('unowned refresh resource exists');
      // The receipt is established before enabling the persistent timer.
      mkdirSync(live, { mode: 0o700 });
      for (const [name, bytes] of Object.entries(sources)) privateWrite(join(live, name), bytes);
      for (const [name, text] of Object.entries(units)) privateWrite(join(unitDir, name), text);
      privateWrite(ownerFile, JSON.stringify(data) + '\n');
      verifyInstalled(data);
    }
    systemctl(['daemon-reload']);
    systemctl(['enable', '--now', timerName]);
    process.stdout.write(`refresh timer installed; owner alex; teardown: ${data.teardown}\n`);
  } else {
    if (!current) fail('refresh timer has no owner receipt');
    verifyInstalled(current);
    if (command === 'status') {
      process.stdout.write(systemctl(['is-enabled', timerName]) + '\n');
      process.stdout.write(systemctl(['is-active', timerName]) + '\n');
    } else {
      systemctl(['disable', '--now', timerName]);
      systemctl(['stop', serviceName]);
      if (command === 'teardown') {
        for (const name of [serviceName, timerName]) rmSync(join(unitDir, name));
        rmSync(live, { recursive: true });
        rmSync(ownerFile);
        systemctl(['daemon-reload']);
      }
      process.stdout.write(`refresh ${command} complete; publication state retained\n`);
    }
  }
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  try { main(process.argv[2]); }
  catch (error) { process.stderr.write(`registry search service: ${error.message}\n`); process.exitCode = 1; }
}
