// This is an exact source identity, not a graph name or a user-supplied label.
export function originForDocument(doc) {
  return doc.domain === 'meaninggraph' &&
    ['meaning_entity', 'meaning_field'].includes(doc.kind) &&
    doc.source_repository === 'meaninggraph/core' ? 'core' : 'public_registry';
}

export function corePriority(doc) {
  return originForDocument(doc) === 'core' ? 1 : 0;
}

export function rankingForDomain(domain) {
  return {
    sort_by: '_text_match:desc,core_priority:desc',
    ...(domain === 'meaninggraph' ? { prioritize_num_matching_fields: false } : {})
  };
}
