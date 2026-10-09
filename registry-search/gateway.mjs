import { productionEngineOrigin } from './engine-policy.mjs';
import { fieldParentKinds, kindPriority, kinds as acceptedKinds, kindsForFilter, originForDocument, rankingForDomain, repositoryTerms, searchFieldsForQuery } from './provenance.mjs';

const domains = new Set(['meaninggraph', 'modelspec', 'ovdb']);
const kinds = new Set(acceptedKinds);
const origins = new Set(['https://meaninggraph.io', 'https://modelspec.org', 'https://directory.openvaultdb.com']);
const display = ['id', 'title', 'kind', 'identifier', 'qualified_name', 'parent_id', 'parent_label', 'canonical_url', 'status', 'native_kind'];

const reply = (status, data, cors = {}) => Response.json(data, { status, headers: { 'cache-control': 'no-store', ...cors } });
async function boundedText(response, limit) {
  if (!response.body) throw new Error('empty search response');
  const reader = response.body.getReader();
  const chunks = [];
  let size = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    size += value.byteLength;
    if (size > limit) { await reader.cancel(); throw new Error('large search response'); }
    chunks.push(value);
  }
  const body = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) { body.set(chunk, offset); offset += chunk.byteLength; }
  return new TextDecoder('utf-8', { fatal: true }).decode(body);
}
function corsFor(request, env) {
  const origin = request.headers.get('origin');
  const staging = env.REGISTRY_SEARCH_MODE === 'staging' ? (env.REGISTRY_STAGING_ORIGINS || '').split(',').filter(Boolean) : [];
  if (!origin || !(origins.has(origin) || staging.includes(origin))) return null;
  return { 'access-control-allow-origin': origin, 'access-control-allow-methods': 'POST, OPTIONS', 'access-control-allow-headers': 'content-type', vary: 'Origin' };
}
async function readBody(request) {
  if (Number(request.headers.get('content-length')) > 4096) throw new Error('large body');
  if (request.headers.get('content-type')?.split(';')[0].trim() !== 'application/json' || !request.body) throw new Error('invalid content type');
  const reader = request.body.getReader();
  const chunks = [];
  let bytes = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    bytes += value.byteLength;
    if (bytes > 4096) { await reader.cancel(); throw new Error('large body'); }
    chunks.push(value);
  }
  const body = new Uint8Array(bytes);
  let offset = 0;
  for (const chunk of chunks) { body.set(chunk, offset); offset += chunk.byteLength; }
  return JSON.parse(new TextDecoder('utf-8', { fatal: true }).decode(body));
}
function parseQuery(value) {
  if (!value || typeof value !== 'object' || Array.isArray(value) || Object.keys(value).some(key => !['q', 'domain', 'kind', 'parent_id', 'page'].includes(key))) throw new Error('invalid body');
  if (typeof value.q !== 'string' || !value.q.trim() || [...value.q].length > 200 || /[\x00-\x1f]/.test(value.q)) throw new Error('invalid q');
  if (!domains.has(value.domain)) throw new Error('invalid domain');
  const requested = value.kind === undefined ? [] : typeof value.kind === 'string' ? [value.kind] : value.kind;
  if (!Array.isArray(requested) || requested.length > kinds.size || requested.some(kind => !kinds.has(kind)) || new Set(requested).size !== requested.length) throw new Error('invalid kind');
  if (value.parent_id !== undefined && (typeof value.parent_id !== 'string' || !/^[a-f0-9]{64}$/.test(value.parent_id))) throw new Error('invalid parent');
  if (value.page !== undefined && (!Number.isSafeInteger(value.page) || value.page < 1 || value.page > 20)) throw new Error('invalid page');
  return { q: value.q.trim(), domain: value.domain, kinds: kindsForFilter(requested), parent: value.parent_id, page: value.page || 1 };
}
function engineOrigin(env) {
  const url = new URL(env.REGISTRY_TYPESENSE_ORIGIN);
  if (url.protocol !== 'https:' || url.username || url.password || url.port || url.pathname !== '/' || url.search || url.hash) throw new Error('invalid origin');
  if (env.REGISTRY_SEARCH_MODE === 'production') return productionEngineOrigin(env);
  return url;
}
function failureReason(error) {
  if (error?.name === 'TimeoutError' || error?.name === 'AbortError') return 'timeout';
  const message = String(error?.message || '');
  if (/\b1042\b/.test(message)) return 'same_zone_worker';
  if (/\b1024\b/.test(message)) return 'cloudflare_ip';
  if (/redirect/i.test(message)) return 'redirect';
  return 'exception';
}
export function createGateway(fetcher = fetch) {
  return {
    async fetch(request, env) {
      const url = new URL(request.url);
      if (url.pathname !== '/v1/registry-search' || url.search) return reply(404, { error: 'invalid_request' });
      const cors = corsFor(request, env);
      if (request.headers.has('origin') && !cors) return reply(403, { error: 'invalid_request' });
      if (request.method === 'OPTIONS') return new Response(null, { status: 204, headers: cors || {} });
      if (request.method !== 'POST') return reply(405, { error: 'invalid_request' }, cors || {});
      if (!['staging', 'production'].includes(env.REGISTRY_SEARCH_MODE)) return reply(503, { error: 'disabled' }, cors || {});
      let query;
      try { query = parseQuery(await readBody(request)); } catch { return reply(400, { error: 'invalid_request' }, cors || {}); }
      if (!env.REGISTRY_RATE_LIMITER?.limit) {
        console.warn('registry-search unavailable', { stage: 'rate_limit', status: null, reason: 'missing_binding' });
        return reply(503, { error: 'unavailable' }, cors || {});
      }
      let stage = 'rate_limit';
      let upstreamStatus = null;
      try {
        const key = `${query.domain}:${request.headers.get('cf-connecting-ip') || 'unknown'}`;
        if (!(await env.REGISTRY_RATE_LIMITER.limit({ key })).success) return reply(429, { error: 'rate_limited' }, cors || {});
        stage = 'engine_config';
        const origin = engineOrigin(env);
        if (!env.REGISTRY_TYPESENSE_SEARCH_KEY) throw new Error('missing search key');
        const filters = [`domain:=${query.domain}`, 'visibility:=public'];
        if (query.kinds.length) filters.push(`kind:=[${query.kinds.join(',')}]`);
        if (query.parent) filters.push(`parent_id:=${query.parent}`);
        const search = {
          collection: 'registry_metadata', q: query.q, ...searchFieldsForQuery(query.q),
          filter_by: filters.join(' && '), page: String(query.page), per_page: '20',
          ...rankingForDomain(query.domain),
          include_fields: display.join(',') + ',description,field_preview,field_count,source_repository,repository_owner,repository_name,repository_full_name,core_priority,kind_priority,generation_id,domain,visibility', highlight_fields: 'none', search_cutoff_ms: '1500'
        };
        const target = new URL('/multi_search', origin);
        stage = 'engine_fetch';
        const response = await fetcher(target, { method: 'POST', redirect: 'manual', signal: AbortSignal.timeout(2500), headers: { 'x-typesense-api-key': env.REGISTRY_TYPESENSE_SEARCH_KEY, 'content-type': 'application/json' }, body: JSON.stringify({ searches: [search] }) });
        upstreamStatus = response.status;
        stage = 'engine_http';
        if (!response.ok || response.redirected) throw new Error('search failed');
        stage = 'engine_body';
        const raw = await boundedText(response, 1024 * 1024);
        stage = 'engine_result';
        const result = JSON.parse(raw).results?.[0];
        if (!Number.isSafeInteger(result.found) || !Array.isArray(result.hits) || result.search_cutoff === true) throw new Error('incomplete search response');
        const hits = result.hits.slice(0, 20).map(hit => {
          const doc = hit.document;
          if (!doc || doc.domain !== query.domain || doc.visibility !== 'public' || !kinds.has(doc.kind) || doc.core_priority !== (originForDocument(doc) === 'core' ? 1 : 0) || doc.kind_priority !== kindPriority(doc)) throw new Error('search scope violation');
          const repository = repositoryTerms(doc);
          if (doc.repository_owner !== repository.repository_owner || doc.repository_name !== repository.repository_name || doc.repository_full_name !== repository.repository_full_name) throw new Error('search provenance mismatch');
          const item = Object.fromEntries(display.filter(field => doc[field] !== undefined).map(field => [field, doc[field]]));
          item.origin = originForDocument(doc);
          item.repository = repository.repository_full_name;
          if (typeof doc.description === 'string') item.description = [...doc.description.trim()].slice(0, 240).join('');
          if (fieldParentKinds.includes(doc.kind)) {
            if (Array.isArray(doc.field_preview)) item.field_preview = doc.field_preview.filter(field => typeof field === 'string').slice(0, 4).map(field => [...field].slice(0, 64).join(''));
            if (Number.isSafeInteger(doc.field_count) && doc.field_count >= 0) item.field_count = doc.field_count;
          }
          return item;
        });
        return reply(200, { hits, found: result.found, page: query.page, generation: result.hits[0]?.document?.generation_id || null }, cors || {});
      } catch (error) {
        // Fixed labels and numeric status only: never log the query, key, headers, body or URL.
        console.warn('registry-search unavailable', { stage, status: Number.isInteger(upstreamStatus) ? upstreamStatus : null, reason: failureReason(error) });
        return reply(503, { error: 'unavailable' }, cors || {});
      }
    }
  };
}
export default createGateway();
