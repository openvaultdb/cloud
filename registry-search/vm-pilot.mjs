#!/usr/bin/env node
// Private, single-host staging pilot. Run as root on the VM.
import { randomBytes } from 'node:crypto';
import { spawnSync } from 'node:child_process';
import { existsSync, lstatSync, mkdirSync, openSync, readFileSync, writeFileSync, closeSync, chmodSync } from 'node:fs';
import { resolve } from 'node:path';

const ROOT = '/opt/datatug/registry-search';
const DATA = `${ROOT}/data`;
const OWNER_FILE = `${ROOT}/owner.json`;
const ENV_FILE = `${ROOT}/typesense.env`;
const SCRIPT_FILE = `${ROOT}/vm-pilot.mjs`;
const NAME = 'registry-search-typesense';
const IMAGE = 'typesense/typesense:30.2@sha256:610f2d34b1f93d00762869da2c67736775e5798d19a2c8b91b014b8a0cc1e110';
const TASK = '01a114c1-e447-7a81-a869-0d82ca2a7747';
const LABELS = Object.freeze({
  'dev.datatug.owner': 'alex',
  'dev.datatug.task': TASK,
  'dev.datatug.service': 'registry-search-vm-pilot',
});
const OWNER = Object.freeze({
  service: NAME,
  operator: 'alex',
  task: TASK,
  image: IMAGE,
  data: DATA,
  bind: '127.0.0.1:8108',
  stop: `node ${SCRIPT_FILE} stop`,
  teardown: `node ${SCRIPT_FILE} teardown`,
  note: 'Teardown removes the container and retains data and credentials.',
});

function fail(message) { throw new Error(message); }

function docker(args, { allowMissing = false } = {}) {
  const result = spawnSync('docker', args, { encoding: 'utf8', timeout: 120_000, maxBuffer: 1024 * 1024, env: { ...process.env, DOCKER_HOST: 'unix:///var/run/docker.sock' } });
  if (result.error) fail(`docker unavailable: ${result.error.code || result.error.message}`);
  if (result.status !== 0) {
    if (allowMissing && result.status === 1 && /No such (object|container)/i.test(result.stderr)) return null;
    fail(`docker ${args[0]} failed (exit ${result.status}): ${result.stderr.trim().slice(0, 500)}`);
  }
  return result.stdout.trim();
}

function inspect() {
  const raw = docker(['container', 'inspect', NAME], { allowMissing: true });
  return raw === null ? null : JSON.parse(raw)[0];
}

function inspectImage() {
  return JSON.parse(docker(['image', 'inspect', IMAGE]))[0];
}

function requireDirectory(path) {
  if (!existsSync(path)) fail(`missing directory: ${path}`);
  const stat = lstatSync(path);
  if (!stat.isDirectory() || stat.isSymbolicLink()) fail(`unsafe directory: ${path}`);
}

function prepareDirectory(path, mode) {
  if (!existsSync(path)) mkdirSync(path, { mode });
  requireDirectory(path);
}

function requirePrivateFile(path) {
  const stat = lstatSync(path);
  if (!stat.isFile() || stat.isSymbolicLink() || stat.uid !== 0 || (stat.mode & 0o077) !== 0) fail(`unsafe ownership or permissions: ${path}`);
}

function ensureOwnership({ create = false } = {}) {
  requireDirectory('/opt');
  if (create) prepareDirectory('/opt/datatug', 0o700);
  else requireDirectory('/opt/datatug');
  if (create && !existsSync(ROOT)) {
    mkdirSync(ROOT, { mode: 0o700 });
    try { writeExclusive(OWNER_FILE, `${JSON.stringify(OWNER, null, 2)}\n`); }
    catch (error) { fail(`owner receipt creation failed: ${error.message}`); }
  }
  requireDirectory(ROOT);
  if (!existsSync(OWNER_FILE)) fail(`unowned path: ${ROOT}`);
  requirePrivateFile(OWNER_FILE);
  let actual;
  try { actual = JSON.parse(readFileSync(OWNER_FILE, 'utf8')); }
  catch { fail('invalid owner receipt'); }
  for (const [key, value] of Object.entries(OWNER)) if (actual[key] !== value) fail(`owner receipt mismatch: ${key}`);
  if (create) prepareDirectory(DATA, 0o700);
  else requireDirectory(DATA);
  const rootStat = lstatSync(ROOT);
  const dataStat = lstatSync(DATA);
  if (rootStat.uid !== 0 || dataStat.uid !== 0 || (rootStat.mode & 0o077) || (dataStat.mode & 0o077)) fail('pilot directories must be root-owned and private');
}

function writeExclusive(path, contents) {
  const descriptor = openSync(path, 'wx', 0o600);
  try { writeFileSync(descriptor, contents); }
  finally { closeSync(descriptor); }
  chmodSync(path, 0o600);
}

function ensureInstalledScript({ create = false } = {}) {
  const source = readFileSync(new URL(import.meta.url));
  if (!existsSync(SCRIPT_FILE)) {
    if (!create) fail('missing installed teardown script');
    writeExclusive(SCRIPT_FILE, source);
  }
  requirePrivateFile(SCRIPT_FILE);
  if (!readFileSync(SCRIPT_FILE).equals(source)) fail('installed pilot script differs from this version');
}

function apiKey({ create = false } = {}) {
  if (!existsSync(ENV_FILE)) {
    if (!create) fail('missing admin key file');
    writeExclusive(ENV_FILE, `TYPESENSE_API_KEY=${randomBytes(48).toString('base64url')}\nTYPESENSE_DATA_DIR=/data\n`);
  }
  requirePrivateFile(ENV_FILE);
  const content = readFileSync(ENV_FILE, 'utf8');
  const match = /^TYPESENSE_API_KEY=([A-Za-z0-9_-]{64})\nTYPESENSE_DATA_DIR=\/data\n$/.exec(content);
  if (!match) fail('invalid admin key file');
  return match[1];
}

export function verifyContainer(container, key, image) {
  if (!container || container.Name !== `/${NAME}` || container.Config?.Image !== IMAGE) fail('container image/name differs from pilot');
  if (!image?.Id || container.Image !== image.Id ||
      JSON.stringify(container.Config?.Entrypoint ?? null) !== JSON.stringify(image.Config?.Entrypoint ?? null) ||
      JSON.stringify(container.Config?.Cmd ?? null) !== JSON.stringify(image.Config?.Cmd ?? null)) fail('container image command differs from pinned image');
  for (const [name, value] of Object.entries(LABELS)) if (container.Config?.Labels?.[name] !== value) fail(`container owner label mismatch: ${name}`);
  const env = container.Config?.Env || [];
  if (!env.includes(`TYPESENSE_API_KEY=${key}`) || !env.includes('TYPESENSE_DATA_DIR=/data')) fail('container environment differs from pilot');
  const host = container.HostConfig || {};
  if (host.NetworkMode !== 'bridge' || host.Privileged !== false || (host.CapAdd?.length ?? 0) !== 0) fail('container network or privilege policy differs from pilot');
  if (host.NanoCpus !== 2_000_000_000 || host.Memory !== 3 * 1024 ** 3 || host.MemorySwap !== 3 * 1024 ** 3) fail('container CPU/memory limits differ from pilot');
  if (host.RestartPolicy?.Name !== 'unless-stopped' || host.LogConfig?.Type !== 'json-file' || host.LogConfig?.Config?.['max-size'] !== '10m' || host.LogConfig?.Config?.['max-file'] !== '3') fail('container restart/log policy differs from pilot');
  const bindings = host.PortBindings || {};
  if (Object.keys(bindings).length !== 1 || JSON.stringify(bindings['8108/tcp']) !== JSON.stringify([{ HostIp: '127.0.0.1', HostPort: '8108' }])) fail('container port binding differs from pilot');
  const mounts = container.Mounts || [];
  if (mounts.length !== 1 || mounts[0].Type !== 'bind' || mounts[0].Source !== DATA || mounts[0].Destination !== '/data' || mounts[0].RW !== true) fail('container data mount differs from pilot');
}

async function readiness(key) {
  for (let attempt = 0; attempt < 30; attempt++) {
    try {
      const health = await fetch('http://127.0.0.1:8108/health', { signal: AbortSignal.timeout(1500) });
      if (health.ok && (await health.json()).ok === true) {
        const debug = await fetch('http://127.0.0.1:8108/debug', { headers: { 'x-typesense-api-key': key }, signal: AbortSignal.timeout(1500) });
        if (debug.ok && (await debug.json()).version === '30.2') return;
      }
    } catch { /* wait for engine readiness without echoing credentials */ }
    await new Promise(resolveWait => setTimeout(resolveWait, 1000));
  }
  fail('Typesense did not become healthy at loopback within 30 seconds');
}

async function main(command) {
  if (process.platform !== 'linux' || process.getuid?.() !== 0) fail('run as root on the Linux VM');
  if (!['install', 'status', 'stop', 'teardown'].includes(command)) fail('usage: node registry-search/vm-pilot.mjs install|status|stop|teardown');
  const creating = command === 'install';
  ensureOwnership({ create: creating });
  ensureInstalledScript({ create: creating });
  const key = apiKey({ create: creating });
  const existing = inspect();
  if (creating && !existing) docker(['image', 'pull', IMAGE]);
  const image = inspectImage();
  if (existing) verifyContainer(existing, key, image);
  if (command === 'install') {
    if (!existing) {
      docker(['container', 'run', '--detach', '--name', NAME,
        ...Object.entries(LABELS).flatMap(([name, value]) => ['--label', `${name}=${value}`]),
        '--cpus', '2', '--memory', '3g', '--memory-swap', '3g',
        '--network', 'bridge',
        '--restart', 'unless-stopped', '--log-driver', 'json-file', '--log-opt', 'max-size=10m', '--log-opt', 'max-file=3',
        '--publish', '127.0.0.1:8108:8108', '--mount', `type=bind,src=${DATA},dst=/data`, '--env-file', ENV_FILE, IMAGE]);
    } else if (!existing.State?.Running) docker(['container', 'start', NAME]);
    try {
      verifyContainer(inspect(), key, image);
      await readiness(key);
    } catch (error) {
      docker(['container', 'stop', NAME]);
      throw error;
    }
    console.log('registry-search Typesense pilot healthy on 127.0.0.1:8108; owner alex; stop: node registry-search/vm-pilot.mjs stop');
  } else if (command === 'status') {
    if (!existing) fail('pilot container is absent');
    console.log(`registry-search Typesense pilot: ${existing.State?.Running ? 'running' : 'stopped'}; bind 127.0.0.1:8108; owner alex`);
    if (existing.State?.Running) await readiness(key);
  } else if (command === 'stop') {
    if (existing?.State?.Running) docker(['container', 'stop', NAME]);
    console.log('registry-search Typesense pilot stopped; data and credentials retained');
  } else {
    if (existing) docker(['container', 'rm', '--force', NAME]);
    console.log('registry-search Typesense pilot container removed; data and credentials retained');
  }
}

if (process.argv[1] && resolve(process.argv[1]) === new URL(import.meta.url).pathname) {
  main(process.argv[2]).catch(error => { console.error(`vm-pilot: ${error.message}`); process.exitCode = 1; });
}
