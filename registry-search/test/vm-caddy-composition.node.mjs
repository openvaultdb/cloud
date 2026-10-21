import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import { applyConfiguration, insertRoute, removeRoute, routeCompositionVersion, upgradeOwnedSource, verifyComposition, verifyUpgradeRecovery } from '../vm-caddy-route.mjs';

const hash = value => createHash('sha256').update(value).digest('hex');
const snippet = readFileSync(new URL('../vm1-search.caddy', import.meta.url), 'utf8');
const before = 'vm1.sneat.dev {\n    route {\n        respond 404\n    }\n}\n';
const source = 'original installed helper';
const initial = insertRoute(before, snippet);
const receipt = { owner: 'alex', installerHash: hash(source), snippetHash: hash(snippet), beforeHash: hash(before), afterHash: hash(initial) };
const project = '        # BEGIN project-search\n        handle /project/* { respond 401 }\n        # END project-search\n';
const composed = initial.replace('    route {\n', '    route {\n' + project) + 'other.example { respond 200 }\n';

test('legacy receipt status accepts additional routes while teardown preserves them', () => {
  assert.equal(routeCompositionVersion, 1);
  assert.equal(verifyComposition(receipt, source, snippet, before, initial), before);
  const expected = before.replace('    route {\n', '    route {\n' + project) + 'other.example { respond 200 }\n';
  assert.equal(verifyComposition(receipt, source, snippet, before, composed), expected);
  assert.equal(removeRoute(composed, snippet), expected);
  assert.equal(receipt.afterHash, hash(initial));
});

test('composition rejects owned block, source, snippet, backup and receipt tampering', () => {
  for (const args of [
    [receipt, source, snippet, before, composed.replace('reverse_proxy 127.0.0.1:8108', 'reverse_proxy 127.0.0.1:9999')],
    [receipt, source + 'drift', snippet, before, composed],
    [receipt, source, snippet + 'drift', before, composed],
    [receipt, source, snippet, before + 'drift', composed],
    [{ ...receipt, afterHash: hash(composed) }, source, snippet, before, composed],
    [receipt, source, snippet, before, composed + '        # END registry-search-live\n'],
  ]) assert.throws(() => verifyComposition(...args), /drift|changed/);
});

test('teardown validates composed candidate and restores current bytes on reload failure', () => {
  const candidate = removeRoute(composed, snippet);
  let live = composed;
  const writes = [];
  let reloads = 0;
  assert.throws(() => applyConfiguration(candidate, composed,
    text => { assert.equal(text, candidate); },
    text => { live = text; writes.push(text); },
    () => { if (++reloads === 1) throw new Error('reload failed'); }), /reload failed/);
  assert.equal(live, composed);
  assert.deepEqual(writes, [candidate, composed]);
  assert.equal(reloads, 2);
});

test('validation failure leaves live configuration untouched', () => {
  let touched = false;
  assert.throws(() => applyConfiguration('candidate', composed, () => { throw new Error('invalid'); }, () => { touched = true; }, () => {}), /invalid/);
  assert.equal(touched, false);
});

test('upgrade archives original helper and preserves ownership and original config receipts', () => {
  const originalReceipt = JSON.stringify(receipt) + '\n';
  let archived;
  let installed;
  let saved;
  const next = upgradeOwnedSource(receipt, 'new helper', source, originalReceipt,
    bytes => { archived = bytes; }, bytes => { installed = bytes; }, bytes => { saved = bytes; });
  assert.equal(archived, source);
  assert.equal(installed, 'new helper');
  assert.deepEqual(JSON.parse(saved), { ...receipt, installerHash: hash('new helper'), routeCompositionVersion: 1 });
  assert.equal(next.afterHash, receipt.afterHash);
  assert.equal(next.beforeHash, receipt.beforeHash);
});

test('upgrade receipt failure restores source and receipt; source drift never writes', () => {
  const originalReceipt = JSON.stringify(receipt) + '\n';
  let installed = source;
  let saved = originalReceipt;
  let receiptWrites = 0;
  assert.throws(() => upgradeOwnedSource(receipt, 'new helper', source, originalReceipt, () => {},
    bytes => { installed = bytes; }, bytes => { if (++receiptWrites === 1) throw new Error('disk full'); saved = bytes; }), /disk full/);
  assert.equal(installed, source);
  assert.equal(saved, originalReceipt);
  assert.throws(() => upgradeOwnedSource(receipt, 'new', 'drifted', originalReceipt, () => assert.fail('unexpected archive'), () => assert.fail('unexpected write'), () => assert.fail('unexpected write')), /source drift/);
});

test('interrupted upgrades accept only attested original or target files for recovery', () => {
  const originalReceipt = JSON.stringify(receipt) + '\n';
  const next = { ...receipt, installerHash: hash('new helper'), routeCompositionVersion };
  const nextReceipt = JSON.stringify(next) + '\n';
  const journal = { receipt: originalReceipt, targetHash: hash('new helper') };
  for (const [liveSource, liveReceipt] of [[source, originalReceipt], ['new helper', originalReceipt], ['new helper', nextReceipt], [source, nextReceipt]]) {
    assert.deepEqual(verifyUpgradeRecovery(journal, 'new helper', receipt, source, snippet, before, composed, liveSource, liveReceipt), receipt);
  }
  assert.throws(() => verifyUpgradeRecovery(journal, 'different upgrade', receipt, source, snippet, before, composed, source, originalReceipt), /identity mismatch/);
  assert.throws(() => verifyUpgradeRecovery(journal, 'new helper', receipt, source, snippet, before, composed, 'foreign helper', originalReceipt), /files drifted/);
  assert.throws(() => verifyUpgradeRecovery(journal, 'new helper', receipt, source, snippet, before, composed, source, '{}'), /files drifted/);
});
