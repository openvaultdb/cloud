// Explicit acquisition profiles; existing installations remain on the three v1 feeds.
export const legacyProfile = 'legacy';
export const combinedProfile = 'combined-v2';
export const combinedManifestFormat = 'registry-search-manifest/v2';
export const combinedExportFormat = 'registry-search-export/v2';
export const combinedDomains = Object.freeze(['meaninggraph', 'modelspec']);
export const combinedURL = 'https://meaninggraph.io/registry-search-combined.json';
export const combinedRepository = 'meaninggraph/registry';
export const directoryURL = 'https://directory.openvaultdb.com/registry-search.json';
export function inputProfile(profile = legacyProfile) {
  if (![legacyProfile, combinedProfile].includes(profile)) throw new Error('unknown registry input profile');
  return profile;
}
export const isCombined = manifest => manifest?.format === combinedManifestFormat;
export const exactDomains = value => Array.isArray(value) && JSON.stringify(value) === JSON.stringify(combinedDomains);
