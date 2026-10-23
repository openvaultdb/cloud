// Reference oracle for test/demodb.test.ts: the demodb.dev/ovdb/v1 handlers of
// demo-db/websites src/worker.ts at 792ce70b3816c66dca766f14ea87c65549e83234
// (origin/main, 2026-10-16), copied verbatim below the marker. The only edits:
// `fetch(` became `backendFetch(` so the test can route the second hop into
// this Worker's cloud.openvaultdb.com handler, and the allowlist is read from
// src/demodb-ovdb.json. Do not "fix" this file: it records what clients got.
/* eslint-disable */
// @ts-nocheck
import demodbOVDB from "../../server/gatewayassets/demodb-ovdb.json";

type OVDBManifest = { localId: string; capabilities: { query: boolean } };
const ovdbIndex = { server: demodbOVDB.server };
const ovdbDatabases = new Map(Object.entries(demodbOVDB.query).map(([localId, query]) => [localId, { localId, capabilities: { query } }]));
const ovdbApiOrigin = 'https://cloud.openvaultdb.com';
let backendFetch: (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

/** The catalogue-host branch of demodb-websites for paths under /ovdb/v1*. */
export async function referenceDemoDB(request: Request, fetcher: typeof backendFetch): Promise<Response> {
  backendFetch = fetcher;
  const url = new URL(request.url);
  if (unsafePath(url.pathname)) return new Response('Bad Request', { status: 400, headers: securityHeaders() });
  if (url.pathname === '/ovdb/v1' || url.pathname === '/ovdb/v1/') return serverApiIndex(request);
  if (url.pathname.startsWith('/ovdb/v1/')) return proxyOVDB(request, url);
  if (request.method !== 'GET' && request.method !== 'HEAD') return new Response('Method Not Allowed', { status: 405, headers: { Allow: 'GET, HEAD' } });
  return notFound();
}

// ---- verbatim from demo-db/websites src/worker.ts ----
function unsafePath(pathname: string): boolean {
  if (/%2f|%5c|%00/i.test(pathname)) return true;
  for (const raw of pathname.split('/')) {
    let segment: string;
    try { segment = decodeURIComponent(raw); } catch { return true; }
    if (segment === '.' || segment === '..' || segment.includes('/') || segment.includes('\\') || segment.includes('\0') || segment.includes('%')) return true;
  }
  return false;
}

function serverApiIndex(request: Request): Response {
  if (request.method === 'OPTIONS') return new Response(null, { status: 204, headers: readonlyCorsHeaders() });
  if (request.method !== 'GET' && request.method !== 'HEAD') return new Response('Method Not Allowed', { status: 405, headers: { ...readonlyCorsHeaders(), Allow: 'GET, HEAD, OPTIONS' } });
  const body = JSON.stringify(ovdbIndex.server);
  return new Response(request.method === 'HEAD' ? null : body, { headers: { ...readonlyCorsHeaders(), 'Content-Type': 'application/json; charset=utf-8', 'Cache-Control': 'public, max-age=300', ...securityHeaders() } });
}

async function proxyOVDB(request: Request, url: URL): Promise<Response> {
  const match = /^\/ovdb\/v1\/databases\/([a-z][a-z0-9-]{0,39})(\/.*)?$/.exec(url.pathname);
  const database = match && ovdbDatabases.get(match[1]);
  if (!database) return notFound();
  const suffix = match[2] ?? '';
  if (suffix === '' || suffix === '/') return databaseApiIndex(request, database);
  const query = suffix === '/tugql' || suffix === '/query';
  const recordRead = /^\/records\/[^/]+(?:\/[^/]+)+$/.test(suffix);
  const readPath = suffix === '/read' || suffix === '/inferred-schema' || recordRead;
  if (!query && !readPath) return notFound();
  if (query && !database.capabilities.query) return notFound();
  const methods = query ? ['GET', 'HEAD', 'POST', 'OPTIONS'] : ['GET', 'HEAD', 'OPTIONS'];
  if (request.method === 'OPTIONS') return new Response(null, { status: 204, headers: apiCorsHeaders(methods) });
  if (['POST', 'PUT', 'PATCH', 'DELETE'].includes(request.method) && !(query && request.method === 'POST')) {
    return new Response('OpenVaultDB is read-only', { status: 403, headers: { ...apiCorsHeaders(methods), Allow: methods.join(', ') } });
  }
  if (!methods.includes(request.method)) {
    return new Response('Method Not Allowed', { status: 405, headers: { ...apiCorsHeaders(methods), Allow: methods.join(', ') } });
  }
  const target = new URL(`/v1/databases/${database.localId}${suffix}${url.search}`, ovdbApiOrigin);
  const headers = new Headers({ Accept: request.headers.get('Accept') ?? 'application/json' });
  let body: ArrayBuffer | undefined;
  if (query) {
    if (request.method === 'POST') {
      const length = Number(request.headers.get('Content-Length'));
      if (Number.isFinite(length) && length > 1 * 1024 * 1024) return new Response('Query request is too large', { status: 413, headers: apiCorsHeaders(methods) });
      const limitedBody = await readLimitedBody(request, 1 * 1024 * 1024);
      if (!limitedBody) return new Response('Query request is too large', { status: 413, headers: apiCorsHeaders(methods) });
      body = limitedBody;
      const contentType = request.headers.get('Content-Type');
      if (contentType) headers.set('Content-Type', contentType);
    }
  }
  for (const name of ['OVDB-Page-Size', 'OVDB-Page-Token', 'OVDB-Page-Close']) {
    const value = request.headers.get(name);
    if (value) headers.set(name, value);
  }
  try {
    const response = await backendFetch(target, { method: request.method, headers, body, redirect: 'manual' });
    if (response.status >= 300 && response.status < 400) return new Response('The OVDB backend returned a redirect', { status: 502, headers: securityHeaders() });
    const outputHeaders = new Headers();
    for (const name of ['Content-Type', 'Cache-Control', 'ETag', 'Last-Modified', 'Vary', 'Link', 'OVDB-Page-Size', 'OVDB-Page-Token']) {
      const value = response.headers.get(name);
      if (value) outputHeaders.set(name, value);
    }
    for (const [name, value] of Object.entries(apiCorsHeaders(methods))) outputHeaders.set(name, value);
    for (const [name, value] of Object.entries(securityHeaders())) outputHeaders.set(name, value);
    return new Response(request.method === 'HEAD' ? null : response.body, { status: response.status, statusText: response.statusText, headers: outputHeaders });
  } catch (error) {
    logGatewayFailure(query ? 'query' : 'read', database, error);
    return new Response('OpenVaultDB is temporarily unavailable', { status: 502, headers: { ...apiCorsHeaders(methods), ...securityHeaders() } });
  }
}

async function databaseApiIndex(request: Request, database: OVDBManifest): Promise<Response> {
  if (request.method === 'OPTIONS') return new Response(null, { status: 204, headers: readonlyCorsHeaders() });
  if (['POST', 'PUT', 'PATCH', 'DELETE'].includes(request.method)) return new Response('OpenVaultDB is read-only', { status: 403, headers: { ...readonlyCorsHeaders(), Allow: 'GET, HEAD, OPTIONS' } });
  if (request.method !== 'GET' && request.method !== 'HEAD') return new Response('Method Not Allowed', { status: 405, headers: { ...readonlyCorsHeaders(), Allow: 'GET, HEAD, OPTIONS' } });
  return fetchDatabaseMetadata(request, database);
}

async function fetchDatabaseMetadata(request: Request, database: OVDBManifest): Promise<Response> {
  const target = new URL(`/v1/databases/${database.localId}`, ovdbApiOrigin);
  try {
    const upstream = await backendFetch(target, { method: request.method, headers: { Accept: 'application/json' }, redirect: 'manual' });
    if (upstream.status >= 300 && upstream.status < 400) return new Response('The OVDB backend returned a redirect', { status: 502, headers: securityHeaders() });
    const outputHeaders = new Headers();
    for (const name of ['Content-Type', 'Cache-Control', 'ETag', 'Last-Modified', 'Vary', 'Link']) {
      const value = upstream.headers.get(name);
      if (value) outputHeaders.set(name, value);
    }
    for (const [name, value] of Object.entries({ ...readonlyCorsHeaders(), 'Access-Control-Expose-Headers': 'Cache-Control, ETag, Last-Modified, Link, Vary', ...securityHeaders() })) outputHeaders.set(name, value);
    if (request.method === 'HEAD' || !upstream.ok) return new Response(request.method === 'HEAD' ? null : upstream.body, { status: upstream.status, statusText: upstream.statusText, headers: outputHeaders });

    const metadata = await upstream.json() as { id?: unknown; endpoints?: { tugql?: unknown }; [key: string]: unknown };
    if (!metadata || metadata.id !== database.localId || typeof metadata.endpoints?.tugql !== 'string') return new Response('The OVDB backend returned invalid database metadata', { status: 502, headers: { ...readonlyCorsHeaders(), ...securityHeaders() } });
    const expectedBackendEndpoint = `https://cloud.openvaultdb.com/v1/databases/${database.localId}/tugql`;
    if (metadata.endpoints.tugql !== expectedBackendEndpoint) return new Response('The OVDB backend returned an unexpected query endpoint', { status: 502, headers: { ...readonlyCorsHeaders(), ...securityHeaders() } });
    metadata.endpoints.tugql = `https://demodb.dev/ovdb/v1/databases/${database.localId}/tugql`;
    outputHeaders.set('Content-Type', 'application/json; charset=utf-8');
    outputHeaders.delete('ETag');
    return new Response(JSON.stringify(metadata), { status: upstream.status, statusText: upstream.statusText, headers: outputHeaders });
  } catch (error) {
    logGatewayFailure('metadata', database, error);
    return new Response('OpenVaultDB is temporarily unavailable', { status: 502, headers: { ...readonlyCorsHeaders(), ...securityHeaders() } });
  }
}

function logGatewayFailure(operation: 'metadata' | 'query' | 'read', database: OVDBManifest, error: unknown): void {
  const errorName = error instanceof Error
    ? error.name.replace(/[^A-Za-z0-9_.-]/g, '').slice(0, 40) || 'Error'
    : 'NonErrorRejection';
  const errorMessage = error instanceof Error
    ? error.message
      .replace(/https?:\/\/[^\s"'<>]+/gi, '[url]')
      .replace(/\b(authorization|cookie|set-cookie|ovdb[-_ ]?page[-_ ]?token|page[-_ ]?token|token|secret|password)\b\s*[:=]\s*(?:Bearer\s+)?(?:"[^"]*"|'[^']*'|[^\s,;]+)/gi, '$1=[redacted]')
      .replace(/\bBearer\s+[A-Za-z0-9._~+/=-]+/gi, 'Bearer [redacted]')
      .replace(/([?&][A-Za-z0-9_.-]+=)[^&#\s]*/g, '$1[redacted]')
      .replace(/[\u0000-\u001f\u007f]/g, ' ')
      .slice(0, 200)
    : 'non-error rejection';
  console.error(JSON.stringify({ event: 'ovdb_gateway_fetch_failed', operation, database: database.localId, errorName, errorMessage }));
}

async function readLimitedBody(request: Request, maximum: number): Promise<ArrayBuffer | null> {
  if (!request.body) return new ArrayBuffer(0);
  const reader = request.body.getReader();
  const chunks: Uint8Array[] = [];
  let size = 0;
  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    size += value.byteLength;
    if (size > maximum) {
      await reader.cancel();
      return null;
    }
    chunks.push(value);
  }
  const combined = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) { combined.set(chunk, offset); offset += chunk.byteLength; }
  return combined.buffer;
}

function apiCorsHeaders(methods: string[]): Record<string, string> {
  const allowed = [...new Set([...methods, 'GET', 'HEAD', 'OPTIONS'])];
  return {
    'Access-Control-Allow-Origin': '*', 'Access-Control-Allow-Methods': allowed.join(', '),
    'Access-Control-Allow-Headers': 'Content-Type, Accept, OVDB-Page-Size, OVDB-Page-Token, OVDB-Page-Close',
    'Access-Control-Expose-Headers': 'Cache-Control, ETag, Last-Modified, Link, OVDB-Page-Size, OVDB-Page-Token, Vary', 'Access-Control-Max-Age': '86400',
  };
}

function notFound(): Response {
  return new Response('Not Found', { status: 404, headers: { 'Content-Type': 'text/plain; charset=utf-8', ...securityHeaders() } });
}

function corsHeaders(): Record<string, string> {
  return { 'Access-Control-Allow-Origin': '*', 'Access-Control-Allow-Methods': 'GET, HEAD, OPTIONS, POST', 'Access-Control-Allow-Headers': 'Content-Type, Accept, OVDB-Page-Size, OVDB-Page-Token, OVDB-Page-Close', 'Access-Control-Max-Age': '86400' };
}
function readonlyCorsHeaders(): Record<string, string> {
  return { 'Access-Control-Allow-Origin': '*', 'Access-Control-Allow-Methods': 'GET, HEAD, OPTIONS', 'Access-Control-Allow-Headers': 'Content-Type', 'Access-Control-Max-Age': '86400' };
}
function securityHeaders(): Record<string, string> {
  return { 'X-Content-Type-Options': 'nosniff', 'Referrer-Policy': 'strict-origin-when-cross-origin' };
}
