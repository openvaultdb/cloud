#!/usr/bin/env node
// Add only the registry search route to the existing VM Caddyfile.
import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { existsSync, lstatSync, mkdirSync, readFileSync, writeFileSync, renameSync, rmSync, chmodSync, chownSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = '/opt/datatug/registry-search';
const installed = join(root, 'caddy-live');
const receiptPath = join(root, 'caddy-owner.json');
const backupPath = join(installed, 'Caddyfile.before-registry-search');
const caddyfile = '/etc/caddy/Caddyfile';
const anchor = 'vm1.sneat.dev {\n    route {\n';
const begin = '        # BEGIN registry-search-live owner=alex task=01a114c1-e447-7a81-a869-0d82ca2a7747\n';
const end = '        # END registry-search-live\n';
const task = '01a114c1-e447-7a81-a869-0d82ca2a7747';

function hash(value) { return createHash('sha256').update(value).digest('hex'); }
function fail(message) { throw new Error(message); }
function rootFile(path, privateFile = false) {
  const stat = lstatSync(path);
  if (!stat.isFile() || stat.isSymbolicLink() || stat.uid !== 0 || privateFile && (stat.mode & 0o077)) fail(`unsafe root file: ${path}`);
  return stat;
}
function rootDir(path) {
  const stat = lstatSync(path);
  if (!stat.isDirectory() || stat.isSymbolicLink() || stat.uid !== 0 || (stat.mode & 0o077)) fail(`unsafe root directory: ${path}`);
}
function privateWrite(path, data) {
  writeFileSync(path, data, { mode: 0o600, flag: 'wx' });
  chmodSync(path, 0o600);
}
function caddy(args) {
  const result = spawnSync('/usr/bin/caddy', args, { encoding: 'utf8', maxBuffer: 4096, timeout: 30_000 });
  if (result.error || result.status !== 0) fail(`Caddy ${args[0]} failed`);
}
function atomicCaddyfile(contents, stat) {
  const temp = `${caddyfile}.registry-search-${process.pid}.tmp`;
  try {
    writeFileSync(temp, contents, { mode: stat.mode & 0o777, flag: 'wx' });
    chmodSync(temp, stat.mode & 0o777);
    chownSync(temp, stat.uid, stat.gid);
    renameSync(temp, caddyfile);
  } finally { if (existsSync(temp)) rmSync(temp); }
}
function validateCandidate(contents, stat) {
  const temp = `${caddyfile}.registry-search-${process.pid}.validate`;
  try {
    writeFileSync(temp, contents, { mode: stat.mode & 0o777, flag: 'wx' });
    chmodSync(temp, stat.mode & 0o777);
    caddy(['validate', '--config', temp, '--adapter', 'caddyfile']);
  } finally { if (existsSync(temp)) rmSync(temp); }
}
export function insertRoute(original, snippet) {
  if (original.split(anchor).length !== 2 || original.includes(begin) || original.includes(end)) throw new Error('VM Caddy anchor missing, repeated or already owned');
  if (!snippet.includes('path /multi_search') || !snippet.includes('method POST') || !snippet.includes('query ""') || !snippet.includes('reverse_proxy 127.0.0.1:8108')) throw new Error('unsafe search snippet');
  return original.replace(anchor, anchor + begin + snippet.trimEnd().split('\n').map(line => `        ${line}`).join('\n') + '\n' + end);
}
export function removeRoute(installedText, snippet) {
  const block = begin + snippet.trimEnd().split('\n').map(line => `        ${line}`).join('\n') + '\n' + end;
  if (installedText.split(block).length !== 2) throw new Error('owned Caddy route changed');
  return installedText.replace(block, '');
}
function owner() {
  if (!existsSync(receiptPath)) return null;
  rootFile(receiptPath, true);
  const value = JSON.parse(readFileSync(receiptPath, 'utf8'));
  if (value.owner !== 'alex' || value.task !== task || value.caddyfile !== caddyfile || value.teardown !== `node ${join(installed, 'vm-caddy-route.mjs')} teardown`) fail('Caddy route owner mismatch');
  return value;
}
function checkInstalled(value) {
  rootDir(installed);
  for (const name of ['vm-caddy-route.mjs', 'vm1-search.caddy', 'Caddyfile.before-registry-search']) rootFile(join(installed, name), true);
  if (hash(readFileSync(join(installed, 'vm-caddy-route.mjs'))) !== value.installerHash || hash(readFileSync(join(installed, 'vm1-search.caddy'))) !== value.snippetHash || hash(readFileSync(backupPath)) !== value.beforeHash) fail('owned Caddy source or backup drift');
  const actual = readFileSync(caddyfile);
  if (hash(actual) !== value.afterHash) fail('Caddyfile changed since registry route install');
  return actual;
}
function main(command) {
  if (process.platform !== 'linux' || process.getuid?.() !== 0) fail('run as root on Linux');
  if (!['install', 'status', 'teardown'].includes(command)) fail('usage: vm-caddy-route.mjs install|status|teardown');
  rootDir(root);
  rootFile(join(root, 'owner.json'), true);
  const pilot = JSON.parse(readFileSync(join(root, 'owner.json'), 'utf8'));
  if (pilot.operator !== 'alex' || pilot.task !== task || pilot.bind !== '127.0.0.1:8108') fail('unexpected VM pilot owner');
  const stat = rootFile(caddyfile);
  const current = owner();
  if (command === 'install') {
    const dir = dirname(fileURLToPath(import.meta.url));
    const source = readFileSync(new URL(import.meta.url));
    const snippet = readFileSync(join(dir, 'vm1-search.caddy'), 'utf8');
    if (current) {
      checkInstalled(current);
      if (current.installerHash !== hash(source) || current.snippetHash !== hash(snippet)) fail('installed Caddy route version differs');
      process.stdout.write(`Caddy route already owned; teardown: ${current.teardown}\n`);
      return;
    }
    if (existsSync(installed) || existsSync(receiptPath)) fail('unowned Caddy resource exists');
    const before = readFileSync(caddyfile, 'utf8');
    const after = insertRoute(before, snippet);
    validateCandidate(before, stat);
    validateCandidate(after, stat);
    mkdirSync(installed, { mode: 0o700 });
    privateWrite(join(installed, 'vm-caddy-route.mjs'), source);
    privateWrite(join(installed, 'vm1-search.caddy'), snippet);
    privateWrite(backupPath, before);
    const data = { owner: 'alex', task, caddyfile, installerHash: hash(source), snippetHash: hash(snippet), beforeHash: hash(before), afterHash: hash(after), teardown: `node ${join(installed, 'vm-caddy-route.mjs')} teardown` };
    privateWrite(receiptPath, JSON.stringify(data) + '\n');
    atomicCaddyfile(after, stat);
    try { caddy(['reload', '--config', caddyfile, '--adapter', 'caddyfile']); }
    catch (error) {
      atomicCaddyfile(before, stat);
      caddy(['reload', '--config', caddyfile, '--adapter', 'caddyfile']);
      rmSync(receiptPath);
      rmSync(installed, { recursive: true });
      throw error;
    }
    process.stdout.write(`Caddy search route installed; owner alex; teardown: ${data.teardown}\n`);
  } else {
    if (!current) fail('Caddy route has no owner receipt');
    const installedText = checkInstalled(current).toString('utf8');
    const snippet = readFileSync(join(installed, 'vm1-search.caddy'), 'utf8');
    const before = readFileSync(backupPath, 'utf8');
    if (removeRoute(installedText, snippet) !== before) fail('Caddy backup does not match owned route removal');
    if (command === 'status') process.stdout.write(`Caddy search route owned by alex; teardown: ${current.teardown}\n`);
    else {
      validateCandidate(before, stat);
      atomicCaddyfile(before, stat);
      try { caddy(['reload', '--config', caddyfile, '--adapter', 'caddyfile']); }
      catch (error) {
        atomicCaddyfile(installedText, stat);
        caddy(['reload', '--config', caddyfile, '--adapter', 'caddyfile']);
        throw error;
      }
      rmSync(receiptPath);
      rmSync(installed, { recursive: true });
      process.stdout.write('Caddy search route removed; original file restored\n');
    }
  }
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  try { main(process.argv[2]); }
  catch (error) { process.stderr.write(`registry search Caddy route: ${error.message}\n`); process.exitCode = 1; }
}
