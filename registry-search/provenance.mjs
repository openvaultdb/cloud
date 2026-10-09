// Every document kind the service accepts. This module has no imports, so the Worker bundle
// (gateway.mjs) and the VM refresh service share one list without pulling in node:crypto.
// model_record is the current name of a ModelSpec record type; model_entity and
// model_collection are the earlier spellings and stay accepted until a later change removes them.
export const kinds = Object.freeze(['meaning_entity', 'meaning_field', 'model', 'model_entity', 'model_record', 'model_collection', 'model_field', 'ovdb_server', 'ovdb_database', 'ovdb_collection']);
// Kinds that hold a field list: they carry field_count and field_preview.
export const fieldParentKinds = Object.freeze(['meaning_entity', 'model_entity', 'model_record', 'model_collection']);
// A request for either spelling of a ModelSpec record type returns both, because during the
// transition one site may export model_record while another still exports model_entity.
const recordTypeKinds = Object.freeze(['model_entity', 'model_record']);

export function kindsForFilter(requested) {
  const expanded = [];
  for (const kind of requested) {
    for (const item of recordTypeKinds.includes(kind) ? recordTypeKinds : [kind]) if (!expanded.includes(item)) expanded.push(item);
  }
  return expanded;
}

// This is an exact source identity, not a graph name or a user-supplied label.
export function originForDocument(doc) {
  return doc.domain === 'meaninggraph' &&
    ['meaning_entity', 'meaning_field'].includes(doc.kind) &&
    doc.source_repository === 'meaninggraph/core' ? 'core' : 'public_registry';
}

export function corePriority(doc) {
  return originForDocument(doc) === 'core' ? 1 : 0;
}

export function kindPriority(doc) {
  if (doc.kind === 'model') return 3;
  if (['meaning_entity', 'model_entity', 'model_record', 'model_collection', 'ovdb_server', 'ovdb_database', 'ovdb_collection'].includes(doc.kind)) return 2;
  return 0;
}

export function repositoryTerms(doc) {
  const match = /^([A-Za-z0-9_.-]+)\/([A-Za-z0-9_.-]+)$/.exec(doc.source_repository);
  if (!match || match[0].length > 160) throw new Error('invalid source repository');
  return { repository_owner: match[1], repository_name: match[2], repository_full_name: match[0] };
}

export const searchFields = Object.freeze({
  query_by: 'identifier,qualified_name,title,aliases,description,repository_full_name,repository_name,repository_owner',
  query_by_weights: '12,10,6,3,1,5,4,2'
});

export function searchFieldsForQuery(q) {
  // A typed org/repo is an explicit repository lookup. Mixed-field tokenization
  // on Typesense 30.2 can otherwise split its slash into incompatible terms.
  if (q.length <= 160 && /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(q)) {
    return { query_by: 'repository_full_name', query_by_weights: '5' };
  }
  return searchFields;
}

export function rankingForDomain(domain) {
  return {
    sort_by: '_text_match:desc,core_priority:desc,kind_priority:desc',
    ...(domain === 'meaninggraph' ? { prioritize_num_matching_fields: false } : {})
  };
}
