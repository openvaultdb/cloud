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
  if (['meaning_entity', 'model_entity', 'model_collection', 'ovdb_server', 'ovdb_database', 'ovdb_collection'].includes(doc.kind)) return 2;
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
