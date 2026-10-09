import assert from 'node:assert/strict';
import { searchFieldsForQuery } from '../provenance.mjs';

// In-memory stand-in for the Typesense engine, shared by the registry tests.
export function fakeEngine(failImport = false) {
  const collections = new Map();
  let alias = null;
  let failDelete = false;
  let otherAlias = null;
  const engine = {
    get failImport() { return failImport; },
    set failImport(value) { failImport = value; },
    set failDelete(value) { failDelete = value; },
    set otherAlias(value) { otherAlias = value; },
    get alias() { return alias; },
    get collections() { return [...collections.keys()]; },
    async api(method, path, body) {
      if (method === 'GET' && path === '/collections') return { status: 200, body: JSON.stringify([...collections.keys()].map(name => ({ name }))) };
      if (method === 'GET' && path === '/aliases') return { status: 200, body: JSON.stringify({ aliases: [...(alias ? [{ name: 'registry_metadata', collection_name: alias }] : []), ...(otherAlias ? [{ name: 'other', collection_name: otherAlias }] : [])] }) };
      if (method === 'GET' && path === '/aliases/registry_metadata') return alias ? { status: 200, body: JSON.stringify({ collection_name: alias }) } : { status: 404, body: '' };
      if (method === 'PUT' && path === '/aliases/registry_metadata') { alias = body.collection_name; return { status: 200, body: '{}' }; }
      if (method === 'POST' && path === '/collections') { collections.set(body.name, []); return { status: 201, body: '{}' }; }
      const collection = path.match(/^\/collections\/([^/]+)/)?.[1];
      if (method === 'DELETE' && collection) { if (failDelete) return { status: 500, body: '{}' }; collections.delete(collection); return { status: 200, body: '{}' }; }
      if (method === 'GET' && path.endsWith('/documents/search?'+path.split('/documents/search?')[1])) {
        const params = new URLSearchParams(path.split('?')[1]);
        const q = params.get('q');
        assert.equal(params.get('query_by'), searchFieldsForQuery(q).query_by);
        assert.equal(params.get('query_by_weights'), searchFieldsForQuery(q).query_by_weights);
        assert.equal(params.get('sort_by'), '_text_match:desc,core_priority:desc,kind_priority:desc');
        assert.equal(params.get('prioritize_num_matching_fields'), params.get('filter_by').includes('domain:=meaninggraph') ? 'false' : null);
        const docs = collections.get(collection) || [];
        return { status: 200, body: JSON.stringify({ hits: docs.filter(doc => params.get('query_by') === 'repository_full_name' ? doc.repository_full_name === q : doc.identifier === q).map(document => ({ document })) }) };
      }
      if (method === 'POST' && path.includes('/documents/import?')) {
        const rows = body.trim().split('\n').map(JSON.parse);
        collections.get(collection).push(...(failImport ? rows.slice(1) : rows));
        return { status: 200, body: rows.map((row, i) => JSON.stringify({ success: !failImport || i > 0, id: row.id })).join('\n') + '\n' };
      }
      if (method === 'GET' && collection) return collections.has(collection) ? { status: 200, body: JSON.stringify({ num_documents: collections.get(collection).length }) } : { status: 404, body: '' };
      throw new Error(`unexpected ${method} ${path}`);
    }
  };
  return engine;
}
