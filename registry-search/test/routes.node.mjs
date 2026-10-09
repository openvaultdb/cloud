import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFile, mkdtemp } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { stableId, validateDocument } from '../schema.mjs';
import { canonicalRoutes, mergeExports } from '../merge.mjs';
import { fetchCurrentCorpus, refresh } from '../refresh.mjs';
import { createGateway } from '../gateway.mjs';
import { fakeEngine } from './fake-engine.mjs';

// Canonical URLs: which routes the validator accepts for each site, and a check against
// each site's real public export so that a route change on a site cannot pass unnoticed.
const sha = char => char.repeat(40);
const sources = {
  meaninggraph: { domain: 'meaninggraph', repository: 'meaninggraph/registry', revision: sha('1') },
  modelspec: { domain: 'modelspec', repository: 'modelspec-org/registry', revision: sha('2') },
  ovdb: { domain: 'ovdb', repository: 'openvaultdb/directory', revision: sha('3') }
};
const revisions = new Map([...Object.values(sources)].map(source => [source.repository, source.revision]));
function doc(domain, kind, canonical_url) {
  const source = sources[domain];
  const native_id = `${domain}/${kind}/thing`;
  return {
    id: stableId(domain, kind, native_id), domain, kind, native_id, title: 'Thing', identifier: 'thing', qualified_name: 'g.thing', canonical_url,
    visibility: 'public', source_repository: source.repository, source_commit: source.revision, source_path: 'model/g.yaml'
  };
}
const accepted = (domain, kind, url) => validateDocument(doc(domain, kind, url), sources[domain], canonicalRoutes, revisions);
const refused = (domain, kind, url) => assert.throws(() => accepted(domain, kind, url), /unsafe canonical URL|Invalid URL/, url);
const repo = 'https://meaninggraph.io/registry/github.com/demo-db/pubs';

test('meaninggraph.io: the repository-qualified route is accepted for each kind that uses it', () => {
  accepted('meaninggraph', 'meaning_entity', `${repo}/entities/author/`);
  accepted('meaninggraph', 'meaning_field', `${repo}/concepts/book-price/`);
  accepted('meaninggraph', 'meaning_entity', 'https://meaninggraph.io/registry/github.com/meaninggraph/core/entities/order/');
  accepted('meaninggraph', 'meaning_field', 'https://meaninggraph.io/registry/github.com/openvaultdb/ovdb/concepts/ecb-quote-currency/');
  accepted('meaninggraph', 'meaning_field', 'https://meaninggraph.io/registry/github.com/a_b.c-d/e.f_g-h/concepts/a1-b2/');
  // A fragment is allowed on the same terms as before.
  accepted('meaninggraph', 'meaning_entity', `${repo}/entities/author/#field-name`);
});

test('meaninggraph.io: the earlier /graphs/<graph>/concepts/<concept>/ route stays accepted', () => {
  accepted('meaninggraph', 'meaning_entity', 'https://meaninggraph.io/graphs/pubs/concepts/author/');
  accepted('meaninggraph', 'meaning_field', 'https://meaninggraph.io/graphs/pubs/concepts/book-price');
  accepted('meaninggraph', 'meaning_entity', 'https://meaninggraph.io/graphs/demo/concepts/Customer/');
});

test('meaninggraph.io: unsafe variants of the repository-qualified route stay refused', () => {
  const bad = [
    // another origin, scheme, port or credentials
    'https://evil.example/registry/github.com/demo-db/pubs/entities/author/',
    'https://meaninggraph.io.evil.example/registry/github.com/demo-db/pubs/entities/author/',
    'https://evil.example@meaninggraph.io/registry/github.com/demo-db/pubs/entities/author/',
    'https://user:pass@meaninggraph.io/registry/github.com/demo-db/pubs/entities/author/',
    'http://meaninggraph.io/registry/github.com/demo-db/pubs/entities/author/',
    'https://meaninggraph.io:8443/registry/github.com/demo-db/pubs/entities/author/',
    'https://modelspec.org/registry/github.com/demo-db/pubs/entities/author/',
    // path traversal, written out or percent-encoded
    'https://meaninggraph.io/registry/github.com/../pubs/entities/author/',
    'https://meaninggraph.io/registry/github.com/demo-db/../entities/author/',
    'https://meaninggraph.io/registry/github.com/%2e%2e/pubs/entities/author/',
    'https://meaninggraph.io/registry/github.com/demo-db/pubs/entities/../../../graphs/x/',
    'https://meaninggraph.io/registry/github.com/demo-db/pubs/entities/other/../author/',
    'https://meaninggraph.io/registry/github.com/demo-db/pubs/entities/%2e%2e/author/',
    'https://meaninggraph.io/registry/github.com/demo-db/pubs/entities/author%2f..%2f..%2f/',
    'https://meaninggraph.io/registry/github.com/./pubs/entities/author/',
    // an extra, missing or empty segment
    `${repo}/entities/author/extra/`,
    `${repo}/extra/entities/author/`,
    'https://meaninggraph.io/registry/github.com/demo-db/entities/author/',
    'https://meaninggraph.io/registry/github.com/demo-db/pubs/entities//',
    'https://meaninggraph.io/registry/github.com//pubs/entities/author/',
    'https://meaninggraph.io/registry/github.com/demo-db/pubs/sub/entities/author/',
    'https://meaninggraph.io/registry/entities/author/',
    // no trailing slash (the site always writes one)
    `${repo}/entities/author`,
    // a query string, or an unsafe fragment
    `${repo}/entities/author/?x=1`,
    `${repo}/entities/author/?`,
    `${repo}/entities/author/#<script>`,
    `${repo}/entities/author/#a b`,
    // upper case, in the fixed words or in the slug
    'https://meaninggraph.io/Registry/github.com/demo-db/pubs/entities/author/',
    'https://meaninggraph.io/registry/GitHub.com/demo-db/pubs/entities/author/',
    `${repo}/Entities/author/`,
    `${repo}/ENTITIES/author/`,
    `${repo}/entities/Author/`,
    `${repo}/concepts/Book-Price/`,
    // percent-encoded characters, in any segment
    'https://meaninggraph.io/registry/github.com/demo-db/pu%62s/entities/author/',
    'https://meaninggraph.io/registry/github.com/demo%2Ddb/pubs/entities/author/',
    'https://meaninggraph.io/registry/github.com/demo-db%2fpubs/entities/author/',
    `${repo}/%65ntities/author/`,
    `${repo}/entities/au%74hor/`,
    `${repo}/entities/author%2f/`,
    `${repo}/entities/%61uthor/`,
    // a different middle word than entities or concepts
    `${repo}/records/author/`,
    `${repo}/entity/author/`,
    `${repo}/concept/author/`,
    `${repo}/graphs/author/`,
    `${repo}/entities-x/author/`,
    `${repo}/xentities/author/`,
    `${repo}/entitiesconcepts/author/`,
    `${repo}/entities|concepts/author/`,
    // a slug the site does not produce
    `${repo}/entities/-author/`,
    `${repo}/entities/author-/`,
    `${repo}/entities/au--thor/`,
    `${repo}/entities/au_thor/`,
    `${repo}/entities/1author/`,
    `${repo}/entities/au.thor/`,
    // a host other than github.com, or a character the site never puts in an owner or repository
    'https://meaninggraph.io/registry/gitlab.com/demo-db/pubs/entities/author/',
    'https://meaninggraph.io/registry/github.com.evil/demo-db/pubs/entities/author/',
    'https://meaninggraph.io/registry/github.com/demo db/pubs/entities/author/',
    'https://meaninggraph.io/registry/github.com/demo-db/pu:bs/entities/author/',
    // a prefix or suffix around the whole path
    'https://meaninggraph.io/x/registry/github.com/demo-db/pubs/entities/author/',
    'https://meaninggraph.io/registry/github.com/demo-db/pubs/entities/author/x/'
  ];
  for (const url of bad) refused('meaninggraph', 'meaning_entity', url);
});

test('modelspec.org: both anchor forms are accepted, the earlier and the one the pages carry now', () => {
  const page = 'https://modelspec.org/registry/models/northwind/';
  accepted('modelspec', 'model', page);
  accepted('modelspec', 'model_entity', `${page}#entity-Products`);
  accepted('modelspec', 'model_field', `${page}#property-Employees-Address`);
  accepted('modelspec', 'model_record', `${page}#record-Products`);
  accepted('modelspec', 'model_field', `${page}#field-Employees-Address`);
  accepted('modelspec', 'model_field', `${page}#field-film_text-title`);
  for (const url of [`${page}?x=1`, `${page}#field-<b>`, 'https://modelspec.org/registry/models/a/b/#record-X', 'https://evil.example/registry/models/northwind/#record-X', 'https://modelspec.org/registry/models/%2e%2e/#record-X']) refused('modelspec', 'model_record', url);
});

test('directory.openvaultdb.com: the routes in use stay accepted and unsafe ones refused', () => {
  accepted('ovdb', 'ovdb_database', 'https://directory.openvaultdb.com/ovdb/demodb.dev/northwind/');
  accepted('ovdb', 'ovdb_collection', 'https://directory.openvaultdb.com/ovdb/demodb.dev/northwind/#recordset-Customers');
  accepted('ovdb', 'ovdb_server', `https://directory.openvaultdb.com/servers/${'a'.repeat(64)}/`);
  for (const url of ['https://directory.openvaultdb.com/ovdb/demodb.dev/northwind/?x=1', 'https://evil.example/ovdb/demodb.dev/northwind/', 'https://directory.openvaultdb.com/servers/abc/']) refused('ovdb', 'ovdb_database', url);
});

test('the new route does not widen the other sites', () => {
  for (const domain of ['modelspec', 'ovdb']) refused(domain, domain === 'modelspec' ? 'model' : 'ovdb_database', `https://${domain === 'modelspec' ? 'modelspec.org' : 'directory.openvaultdb.com'}/registry/github.com/demo-db/pubs/entities/author/`);
  refused('meaninggraph', 'meaning_entity', 'https://modelspec.org/graphs/pubs/concepts/author/');
});

test('the gateway returns a hit that carries the repository-qualified URL unchanged', async () => {
  const env = { REGISTRY_SEARCH_MODE: 'staging', REGISTRY_TYPESENSE_ORIGIN: 'https://engine.example/', REGISTRY_TYPESENSE_SEARCH_KEY: 'secret', REGISTRY_RATE_LIMITER: { limit: async () => ({ success: true }) } };
  const urls = [`${repo}/entities/author/`, `${repo}/concepts/book-price/`, 'https://meaninggraph.io/graphs/pubs/concepts/author/'];
  const worker = createGateway(async () => Response.json({ results: [{ found: urls.length, hits: urls.map((canonical_url, i) => ({ document: {
    id: String(i), domain: 'meaninggraph', visibility: 'public', title: 'Author', kind: i === 1 ? 'meaning_field' : 'meaning_entity', canonical_url, generation_id: 'gen',
    source_repository: 'demo-db/pubs', repository_owner: 'demo-db', repository_name: 'pubs', repository_full_name: 'demo-db/pubs', core_priority: 0, kind_priority: i === 1 ? 0 : 2
  } })) }] }));
  const response = await worker.fetch(new Request('https://search.example/v1/registry-search', { method: 'POST', headers: { 'content-type': 'application/json', origin: 'https://meaninggraph.io' }, body: JSON.stringify({ q: 'author', domain: 'meaninggraph' }) }), env);
  assert.equal(response.status, 200);
  assert.deepEqual((await response.json()).hits.map(hit => hit.canonical_url), urls);
});

// test/fixtures/<host>.json is a copy of each site's public /registry-search.json taken on
// 2026-10-09, trimmed to a few documents per kind: meaninggraph.io 8 of 95 (entities and
// concepts, from meaninggraph/core and demo-db/pubs), modelspec.org 8 of 1044 (model,
// model_entity, model_field) and directory.openvaultdb.com 6 of 135 (server, database,
// collection). The documents and the envelope fields, including every source pin, are
// verbatim; only entries of `documents` were dropped. When a site changes a route, update the
// copy from the live export and let this test show whether the validator still accepts it.
const here = dirname(fileURLToPath(import.meta.url));
const hosts = { meaninggraph: 'meaninggraph.io', modelspec: 'modelspec.org', ovdb: 'directory.openvaultdb.com' };
const live = Object.fromEntries(await Promise.all(Object.entries(hosts).map(async ([domain, host]) => [domain, JSON.parse(await readFile(join(here, 'fixtures', `${host}.json`), 'utf8'))])));
const fetcher = async url => {
  const domain = Object.keys(hosts).find(key => url === `https://${hosts[key]}/registry-search.json`);
  const response = Response.json(live[domain]);
  Object.defineProperty(response, 'url', { value: url });
  return response;
};

test('the trimmed copies of the three live exports pass the refresh, validation and merge', async () => {
  const { manifest, exports } = await fetchCurrentCorpus(fetcher);
  assert.deepEqual(exports.map(envelope => [envelope.domain, envelope.fixture, envelope.documents.length]), [['meaninggraph', false, 8], ['modelspec', false, 8], ['ovdb', false, 6]]);
  const merged = mergeExports(manifest, exports);
  assert.equal(merged.documents.length, 22);
  const engine = fakeEngine();
  const result = await refresh({ stateDir: await mkdtemp(join(tmpdir(), 'registry-routes-')), api: engine.api, fetcher });
  assert.deepEqual([result.count, result.generation], [22, merged.generation]);
  assert.equal(engine.alias, `registry_metadata_${merged.generation}`);
});

test('the trimmed copies cover every URL shape the sites publish', () => {
  const shape = ({ domain, kind, canonical_url }) => {
    const url = new URL(canonical_url);
    const path = url.pathname
      .replace(/^\/registry\/github\.com\/[^/]+\/[^/]+\/(entities|concepts)\/[^/]+\/$/, '/registry/github.com/<>/<>/$1/<>/')
      .replace(/^\/registry\/models\/[^/]+\/$/, '/registry/models/<>/')
      .replace(/^\/ovdb\/[^/]+\/[^/]+\/$/, '/ovdb/<>/<>/')
      .replace(/^\/servers\/[a-f0-9]{64}\/$/, '/servers/<server>/');
    return `${domain} ${kind} ${path}${url.hash ? ` #${url.hash.slice(1).split('-')[0]}-…` : ''}`;
  };
  const shapes = new Set(Object.values(live).flatMap(envelope => envelope.documents.map(shape)));
  assert.deepEqual([...shapes].sort(), [
    'meaninggraph meaning_entity /registry/github.com/<>/<>/entities/<>/',
    'meaninggraph meaning_field /registry/github.com/<>/<>/concepts/<>/',
    'modelspec model /registry/models/<>/',
    'modelspec model_entity /registry/models/<>/ #entity-…',
    'modelspec model_field /registry/models/<>/ #property-…',
    'ovdb ovdb_collection /ovdb/<>/<>/ #recordset-…',
    'ovdb ovdb_database /ovdb/<>/<>/',
    'ovdb ovdb_server /servers/<server>/'
  ].sort());
});
