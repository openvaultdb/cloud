const domains = new Set(['meaninggraph', 'modelspec', 'ovdb']);
const kinds = new Set(['meaning_entity', 'meaning_field', 'model', 'model_entity', 'model_collection', 'model_field', 'ovdb_server', 'ovdb_database', 'ovdb_collection']);
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
  if (!Array.isArray(requested) || requested.length > 9 || requested.some(kind => !kinds.has(kind)) || new Set(requested).size !== requested.length) throw new Error('invalid kind');
  if (value.parent_id !== undefined && (typeof value.parent_id !== 'string' || !/^[a-f0-9]{64}$/.test(value.parent_id))) throw new Error('invalid parent');
  if (value.page !== undefined && (!Number.isSafeInteger(value.page) || value.page < 1 || value.page > 20)) throw new Error('invalid page');
  return { q: value.q.trim(), domain: value.domain, kinds: requested, parent: value.parent_id, page: value.page || 1 };
}
function engineOrigin(env) {
  const url = new URL(env.REGISTRY_TYPESENSE_ORIGIN);
  if (url.protocol !== 'https:' || url.username || url.password || url.port || url.pathname !== '/' || url.search || url.hash) throw new Error('invalid origin');
  if (env.REGISTRY_SEARCH_MODE === 'production' && (env.REGISTRY_TYPESENSE_DEPLOYMENT !== 'cloud' || !env.REGISTRY_TYPESENSE_CLOUD_HOST || url.hostname !== env.REGISTRY_TYPESENSE_CLOUD_HOST)) throw new Error('cloud origin required');
  return url;
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
      if (!env.REGISTRY_RATE_LIMITER?.limit) return reply(503, { error: 'unavailable' }, cors || {});
      try {
        const key = `${query.domain}:${request.headers.get('cf-connecting-ip') || 'unknown'}`;
        if (!(await env.REGISTRY_RATE_LIMITER.limit({ key })).success) return reply(429, { error: 'rate_limited' }, cors || {});
        const origin = engineOrigin(env);
        if (!env.REGISTRY_TYPESENSE_SEARCH_KEY) throw new Error('missing search key');
        const filters = [`domain:=${query.domain}`, 'visibility:=public'];
        if (query.kinds.length) filters.push(`kind:=[${query.kinds.join(',')}]`);
        if (query.parent) filters.push(`parent_id:=${query.parent}`);
        const search = {
          collection: 'registry_metadata', q: query.q, query_by: 'identifier,qualified_name,title,aliases,description', query_by_weights: '12,10,6,3,1',
          filter_by: filters.join(' && '), page: String(query.page), per_page: '20',
          include_fields: display.join(',') + ',generation_id,domain,visibility', highlight_fields: 'none', search_cutoff_ms: '1500'
        };
        const target = new URL('/multi_search', origin);
        const response = await fetcher(target, { method: 'POST', redirect: 'error', signal: AbortSignal.timeout(2500), headers: { 'x-typesense-api-key': env.REGISTRY_TYPESENSE_SEARCH_KEY, 'content-type': 'application/json' }, body: JSON.stringify({ searches: [search] }) });
        if (!response.ok || response.redirected) throw new Error('search failed');
        const raw = await boundedText(response, 1024 * 1024);
        const result = JSON.parse(raw).results?.[0];
        if (!Number.isSafeInteger(result.found) || !Array.isArray(result.hits) || result.search_cutoff === true) throw new Error('incomplete search response');
        const hits = result.hits.slice(0, 20).map(hit => {
          const doc = hit.document;
          if (!doc || doc.domain !== query.domain || doc.visibility !== 'public') throw new Error('search scope violation');
          return Object.fromEntries(display.filter(field => doc[field] !== undefined).map(field => [field, doc[field]]));
        });
        return reply(200, { hits, found: result.found, page: query.page, generation: result.hits[0]?.document?.generation_id || null }, cors || {});
      } catch { return reply(503, { error: 'unavailable' }, cors || {}); }
    }
  };
}
export default createGateway();
