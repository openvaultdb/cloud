import { spawn } from 'node:child_process';
import { fstatSync, statSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const helper = fileURLToPath(new URL('./lock_exec.py', import.meta.url));

// Python acquires flock and execs Node in the same PID. The publisher, not an
// outer wrapper, owns the inherited FD for its entire lifetime.
export async function runUnderPublicationLock(lockPath, command, args, env = process.env, stdio = 'inherit') {
  if (!['darwin', 'linux'].includes(process.platform)) throw new Error('unsupported publication lock platform');
  return await new Promise((resolve, reject) => {
    const child = spawn('python3', [helper, lockPath, command, ...args], { env, stdio });
    child.once('error', reject);
    child.once('exit', (code, signal) => resolve(signal ? 1 : code ?? 1));
  });
}

export function assertPublicationLock(lockPath, env = process.env) {
  const fd = Number(env.REGISTRY_SEARCH_LOCK_FD);
  const pid = Number(env.REGISTRY_SEARCH_LOCK_PID);
  if (!Number.isSafeInteger(fd) || fd < 3 || pid !== process.pid) throw new Error('publisher lock not held');
  let descriptor;
  let path;
  try { descriptor = fstatSync(fd); path = statSync(lockPath); }
  catch { throw new Error('publisher lock descriptor missing'); }
  if (!descriptor.isFile() || descriptor.dev !== path.dev || descriptor.ino !== path.ino) throw new Error('publisher lock descriptor mismatch');
}
