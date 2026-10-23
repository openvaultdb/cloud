export function devicePresentation(authorization) {
  const device = authorization?.device || {};
  const clientName = clean(authorization?.client?.name) || "Command-line client";
  const deviceName = clean(device.name);
  const clientVersion = clean(device.client_version);
  const title = deviceName || clientName;
  const details = [];

  if (deviceName) {
    details.push(clientVersion ? `${clientName} ${clientVersion}` : clientName);
  } else if (clientVersion) {
    details.push(`Version ${clientVersion}`);
  }

  const platform = platformPresentation(device.os, device.arch);
  if (platform) details.push(platform);
  return { title, details: details.join(" · ") };
}

export function platformPresentation(rawOS, rawArch) {
  const os = clean(rawOS).toLowerCase();
  const arch = clean(rawArch).toLowerCase();
  const osLabel =
    { darwin: "macOS", linux: "Linux", windows: "Windows", freebsd: "FreeBSD" }[os] ||
    clean(rawOS);
  let archLabel =
    { arm64: "ARM64", amd64: "x86-64", "386": "x86" }[arch] || clean(rawArch);
  if (os === "darwin" && arch === "arm64") archLabel = "Apple silicon";
  return [osLabel, archLabel].filter(Boolean).join(" · ");
}

export function formatDeviceDate(value) {
  if (!value) return "Never";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "Unknown";
  return new Intl.DateTimeFormat(undefined, {
    dateStyle: "medium",
    timeStyle: "short",
  }).format(date);
}

function clean(value) {
  return String(value || "").trim();
}
