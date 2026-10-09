import { createHash } from 'node:crypto';
import { kinds } from './provenance.mjs';

export const domains = ['meaninggraph', 'modelspec', 'ovdb'];
export { kinds };
const required = ['id', 'domain', 'kind', 'native_id', 'title', 'identifier', 'qualified_name', 'canonical_url', 'visibility', 'source_repository', 'source_commit', 'source_path'];
const optionalStrings = ['description', 'parent_id', 'parent_label', 'status', 'native_kind', 'declaring_component'];
const optionalArrays = ['aliases', 'related_ids'];
const allowed = new Set([...required, ...optionalStrings, ...optionalArrays]);
const sha = /^[a-f0-9]{40}$/;
// The repository-qualified route meaninggraph.io publishes since 2026-10-07:
// /registry/github.com/<owner>/<repo>/(entities|concepts)/<slug>/. The segment classes are the
// site's own (its concept-id rule, and the safe-destination check in its search client). The
// older /graphs/<graph>/concepts/<concept>/ form stays accepted in validateDocument.
// URL parsing resolves dot segments, so the new form is also required to be written as it parses.
const meaninggraphRegistryPath = /^\/registry\/github\.com\/(?!\.{1,2}\/)[A-Za-z0-9_.-]+\/(?!\.{1,2}\/)[A-Za-z0-9_.-]+\/(?:entities|concepts)\/[a-z][a-z0-9]*(?:-[a-z][a-z0-9]*)*\/$/;

export function stableId(domain, kind, nativeId) {
  return createHash('sha256').update(JSON.stringify(['public', domain, kind, nativeId])).digest('hex');
}

const isWrittenAsParsed = (raw, url) => raw.split('#')[0] === `${url.origin}${url.pathname}`;

export function validateDocument(doc, source, routes, revisions) {
  if (!doc || typeof doc !== 'object' || Array.isArray(doc)) throw new Error('document must be an object');
  for (const key of Object.keys(doc)) if (!allowed.has(key)) throw new Error(`unknown document field ${key}`);
  for (const key of required) if (typeof doc[key] !== 'string' || !doc[key]) throw new Error(`invalid ${key}`);
  for (const key of optionalStrings) if (key in doc && typeof doc[key] !== 'string') throw new Error(`invalid ${key}`);
  for (const key of optionalArrays) if (key in doc && (!Array.isArray(doc[key]) || doc[key].some(value => typeof value !== 'string' || !value))) throw new Error(`invalid ${key}`);
  if (doc.visibility !== 'public' || !domains.includes(doc.domain) || !kinds.includes(doc.kind)) throw new Error('invalid public scope');
  if (doc.domain !== source.domain || revisions.get(doc.source_repository) !== doc.source_commit || !sha.test(doc.source_commit)) throw new Error('source provenance mismatch');
  if (doc.id !== stableId(doc.domain, doc.kind, doc.native_id)) throw new Error('unstable document id');
  if (!doc.source_path || doc.source_path.startsWith('/') || doc.source_path.split('/').includes('..') || /[\x00-\x1f]/.test(doc.source_path)) throw new Error('unsafe source path');
  const url = new URL(doc.canonical_url);
  const route = routes[doc.domain];
  const matchesRoute = doc.domain === 'meaninggraph' ? /^\/graphs\/[^/]+\/concepts\/[^/]+\/?$/.test(url.pathname) || meaninggraphRegistryPath.test(url.pathname) && isWrittenAsParsed(doc.canonical_url, url)
    : doc.domain === 'modelspec' ? /^\/registry\/models\/[^/]+\/?$/.test(url.pathname)
      : /^\/ovdb\/[^/]+(?:\/[^/]*)*$/.test(url.pathname) || /^\/databases\/[^/]+\/?$/.test(url.pathname) || /^\/servers\/[a-f0-9]{64}\/?$/.test(url.pathname);
  if (!route || url.protocol !== 'https:' || url.username || url.password || url.port || url.search || !route.hosts.includes(url.hostname) || !matchesRoute || url.hash && !/^#[A-Za-z0-9._:-]+$/.test(url.hash)) throw new Error('unsafe canonical URL');
  return doc;
}

export const collectionSchema = name => ({
  name,
  fields: [
    { name: 'domain', type: 'string', facet: true },
    { name: 'kind', type: 'string', facet: true },
    { name: 'visibility', type: 'string', facet: true },
    { name: 'parent_id', type: 'string', facet: true, optional: true },
    { name: 'identifier', type: 'string' },
    { name: 'qualified_name', type: 'string' },
    { name: 'title', type: 'string' },
    { name: 'aliases', type: 'string[]', optional: true },
    { name: 'description', type: 'string', optional: true },
    { name: 'repository_owner', type: 'string', symbols_to_index: ['-'] },
    { name: 'repository_name', type: 'string', symbols_to_index: ['-'] },
    { name: 'repository_full_name', type: 'string', symbols_to_index: ['/', '-'] },
    { name: 'core_priority', type: 'int32' },
    { name: 'kind_priority', type: 'int32' },
    { name: 'field_preview', type: 'string[]', optional: true, index: false },
    { name: 'field_count', type: 'int32', optional: true, index: false },
    { name: 'generation_id', type: 'string', optional: true, index: false },
    { name: 'source_repository', type: 'string', optional: true, index: false },
    { name: 'source_commit', type: 'string', optional: true, index: false },
    { name: 'source_path', type: 'string', optional: true, index: false },
    { name: 'canonical_url', type: 'string', optional: true, index: false },
    { name: 'native_id', type: 'string', optional: true, index: false },
    { name: 'parent_label', type: 'string', optional: true, index: false },
    { name: 'related_ids', type: 'string[]', optional: true, index: false },
    { name: 'status', type: 'string', optional: true, index: false },
    { name: 'native_kind', type: 'string', optional: true, index: false },
    { name: 'declaring_component', type: 'string', optional: true, index: false }
  ]
});
