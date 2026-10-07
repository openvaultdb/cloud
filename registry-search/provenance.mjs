// This is an exact source identity, not a graph name or a user-supplied label.
export function originForDocument(doc) {
  return doc.domain === 'meaninggraph' &&
    ['meaning_entity', 'meaning_field'].includes(doc.kind) &&
    doc.source_repository === 'meaninggraph/core' ? 'core' : 'public_registry';
}

export function corePriority(doc) {
  return originForDocument(doc) === 'core' ? 1 : 0;
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

export function rankingForDomain(domain) {
  return {
    sort_by: '_text_match:desc,core_priority:desc',
    ...(domain === 'meaninggraph' ? { prioritize_num_matching_fields: false } : {})
  };
}
