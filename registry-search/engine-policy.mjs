export function productionEngineOrigin(env) {
  const url = new URL(env.REGISTRY_TYPESENSE_ORIGIN);
  if (url.protocol !== 'https:' || url.username || url.password || url.port || url.pathname !== '/' || url.search || url.hash) throw new Error('invalid engine origin');
  if (env.REGISTRY_TYPESENSE_DEPLOYMENT === 'vm-pilot') {
    if (url.hostname !== 'vm1.sneat.dev') throw new Error('unexpected VM engine host');
  } else if (env.REGISTRY_TYPESENSE_DEPLOYMENT === 'cloud') {
    if (!env.REGISTRY_TYPESENSE_CLOUD_HOST || url.hostname !== env.REGISTRY_TYPESENSE_CLOUD_HOST) throw new Error('unexpected cloud engine host');
  } else throw new Error('unsupported production deployment');
  return url;
}
