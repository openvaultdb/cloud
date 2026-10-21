import {
  accountName,
  auth,
  authenticateWithEmail,
  authenticateWithProvider,
  onAuthStateChanged,
  signOut,
} from "/firebase-auth.js";
import { devicePresentation, formatDeviceDate } from "/device-presentation.js";

const elements = {
  accountName: document.querySelector("[data-account-name]"),
  devices: document.querySelector("[data-devices]"),
  deviceList: document.querySelector("[data-device-list]"),
  emailForm: document.querySelector("[data-email-form]"),
  empty: document.querySelector("[data-empty]"),
  error: document.querySelector("[data-error]"),
  loading: document.querySelector("[data-loading]"),
  signIn: document.querySelector("[data-sign-in]"),
  truncated: document.querySelector("[data-truncated]"),
};

let currentUser = null;
let busy = false;

document.querySelectorAll("[data-provider]").forEach((button) => {
  button.addEventListener("click", () => signInWithProvider(button.dataset.provider));
});
elements.emailForm.addEventListener("submit", signInWithEmail);
document.querySelector("[data-sign-out]").addEventListener("click", () => signOut(auth));

onAuthStateChanged(auth, async (user) => {
  currentUser = user;
  showError("");
  elements.loading.hidden = true;
  elements.signIn.hidden = Boolean(user);
  elements.devices.hidden = !user;
  if (!user) {
    elements.deviceList.replaceChildren();
    return;
  }
  elements.accountName.textContent = accountName(user);
  await loadDevices();
});

async function signInWithProvider(providerName) {
  setBusy(true);
  showError("");
  try {
    await authenticateWithProvider(providerName);
  } catch (error) {
    showError(friendlyError(error));
  } finally {
    setBusy(false);
  }
}

async function signInWithEmail(event) {
  event.preventDefault();
  const values = new FormData(elements.emailForm);
  setBusy(true);
  showError("");
  try {
    await authenticateWithEmail(
      String(values.get("email") || "").trim(),
      String(values.get("password") || ""),
    );
  } catch (error) {
    showError(friendlyError(error));
  } finally {
    setBusy(false);
  }
}

async function loadDevices() {
  if (!currentUser) return;
  setBusy(true);
  showError("");
  try {
    const response = await authenticatedFetch("/api/devices");
    const body = await readResponseBody(response);
    if (!response.ok) throw new Error(body.error || "Authorized devices could not be loaded.");
    const devices = Array.isArray(body.devices) ? body.devices : [];
    elements.deviceList.replaceChildren(...devices.map(renderDevice));
    elements.empty.hidden = devices.length !== 0;
    elements.truncated.hidden = !body.has_more;
  } catch (error) {
    showError(friendlyError(error));
  } finally {
    setBusy(false);
  }
}

function renderDevice(device) {
  const presentation = devicePresentation(device);
  const article = document.createElement("article");
  article.className = "device-item";

  const heading = document.createElement("div");
  heading.className = "device-item-heading";
  const names = document.createElement("div");
  const title = document.createElement("h2");
  title.textContent = presentation.title;
  names.append(title);
  if (presentation.details) {
    const details = document.createElement("p");
    details.className = "device-meta";
    details.textContent = presentation.details;
    names.append(details);
  }
  const status = document.createElement("span");
  status.className = `status-chip status-${device.status || "unknown"}`;
  status.textContent = statusLabel(device.status);
  heading.append(names, status);

  const scopes = document.createElement("div");
  scopes.className = "device-scopes";
  const scopeLabel = document.createElement("strong");
  scopeLabel.textContent = "Permissions";
  const scopeText = document.createElement("span");
  scopeText.textContent = (device.scopes || []).map((scope) => scope.name).join(", ") || "None";
  scopes.append(scopeLabel, scopeText);

  const times = document.createElement("dl");
  times.className = "device-times";
  appendTime(times, "Authorized", device.authorized_at);
  appendTime(times, "Last used", device.last_used_at);
  appendTime(times, "Expires", device.expires_at);

  article.append(heading, scopes, times);
  if (device.can_revoke) {
    const revoke = document.createElement("button");
    revoke.className = "button danger";
    revoke.type = "button";
    revoke.textContent = "Revoke access";
    revoke.addEventListener("click", () => revokeDevice(device, presentation.title));
    article.append(revoke);
  }
  return article;
}

function appendTime(list, label, value) {
  const term = document.createElement("dt");
  const detail = document.createElement("dd");
  term.textContent = label;
  detail.textContent = formatDeviceDate(value);
  list.append(term, detail);
}

async function revokeDevice(device, title) {
  if (!currentUser || busy) return;
  if (!window.confirm(`Revoke OpenVaultDB access for ${title}?`)) return;
  setBusy(true);
  showError("");
  try {
    const response = await authenticatedFetch("/api/devices/revoke", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ device_id: device.id }),
    });
    const body = await readResponseBody(response);
    if (!response.ok) throw new Error(body.error || "The device could not be revoked.");
    await loadDevices();
  } catch (error) {
    showError(friendlyError(error));
  } finally {
    setBusy(false);
  }
}

async function authenticatedFetch(path, init = {}) {
  const idToken = await currentUser.getIdToken(true);
  const headers = new Headers(init.headers || {});
  headers.set("Accept", "application/json");
  headers.set("Authorization", `Bearer ${idToken}`);
  return fetch(path, { ...init, headers });
}

function setBusy(value) {
  busy = value;
  document.querySelectorAll("button, input").forEach((element) => {
    element.disabled = value;
  });
}

function showError(message) {
  elements.error.textContent = message;
  elements.error.hidden = !message;
}

function statusLabel(status) {
  return { active: "Active", revoked: "Revoked", expired: "Expired" }[status] || "Unknown";
}

function friendlyError(error) {
  const code = error && error.code ? String(error.code) : "";
  if (code.includes("popup-closed")) return "Sign-in was cancelled.";
  if (code.includes("invalid-credential") || code.includes("wrong-password")) {
    return "Incorrect email or password.";
  }
  return error instanceof Error ? error.message : "Something went wrong. Please try again.";
}

async function readResponseBody(response) {
  const text = await response.text();
  if (!text) return {};
  try {
    return JSON.parse(text);
  } catch {
    return { error: text };
  }
}
