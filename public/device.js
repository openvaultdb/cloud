import { initializeApp } from "https://www.gstatic.com/firebasejs/12.14.0/firebase-app.js";
import {
  GithubAuthProvider,
  GoogleAuthProvider,
  browserLocalPersistence,
  createUserWithEmailAndPassword,
  getAuth,
  onAuthStateChanged,
  setPersistence,
  signInWithEmailAndPassword,
  signInWithPopup,
  signOut,
} from "https://www.gstatic.com/firebasejs/12.14.0/firebase-auth.js";
import { deviceView } from "/device-view.js";

const firebaseConfig = {
  apiKey: "AIzaSyCeQu1WC182yD0VHrRm4nHUxVf27fY-MLQ",
  authDomain: "auth.sneat.co",
  projectId: "sneat-eur3-1",
  messagingSenderId: "588648831063",
  appId: "1:588648831063:web:303af7e0c5f8a7b10d6b12",
};

const auth = getAuth(initializeApp(firebaseConfig));
await setPersistence(auth, browserLocalPersistence).catch(() => undefined);

const elements = {
  accountName: document.querySelector("[data-account-name]"),
  clientName: document.querySelector("[data-client-name]"),
  codeDisplay: document.querySelector("[data-code-display]"),
  codeForm: document.querySelector("[data-code-form]"),
  codeInput: document.querySelector("[data-code-input]"),
  codeSubmit: document.querySelector("[data-code-submit]"),
  complete: document.querySelector("[data-complete]"),
  completeMessage: document.querySelector("[data-complete-message]"),
  completeTitle: document.querySelector("[data-complete-title]"),
  consent: document.querySelector("[data-consent]"),
  emailForm: document.querySelector("[data-email-form]"),
  emailSubmit: document.querySelector("[data-email-submit]"),
  emailToggle: document.querySelector("[data-email-toggle]"),
  error: document.querySelector("[data-error]"),
  request: document.querySelector("[data-request]"),
  scopeList: document.querySelector("[data-scope-list]"),
  signIn: document.querySelector("[data-sign-in]"),
};

let currentAuthorization = null;
let currentUser = null;
let emailMode = "signin";
let busy = false;

elements.codeInput.addEventListener("input", () => {
  elements.codeInput.value = formatCode(elements.codeInput.value);
});
elements.codeForm.addEventListener("submit", async (event) => {
  event.preventDefault();
  await lookupAuthorization(elements.codeInput.value);
});
document.querySelectorAll("[data-provider]").forEach((button) => {
  button.addEventListener("click", () => signInWithProvider(button.dataset.provider));
});
elements.emailForm.addEventListener("submit", signInWithEmail);
elements.emailToggle.addEventListener("click", toggleEmailMode);
document.querySelector("[data-sign-out]").addEventListener("click", () => signOut(auth));
document.querySelectorAll("[data-decision]").forEach((button) => {
  button.addEventListener("click", () => decide(button.dataset.decision));
});

onAuthStateChanged(auth, (user) => {
  currentUser = user;
  renderNextStep();
});

const initialCode = new URL(location.href).searchParams.get("user_code");
if (initialCode) {
  elements.codeInput.value = formatCode(initialCode);
  await lookupAuthorization(initialCode);
} else {
  elements.codeInput.focus();
}

async function lookupAuthorization(rawCode) {
  const userCode = normalizeCode(rawCode);
  if (userCode.length !== 8) {
    showError("Enter the complete eight-character code from your terminal.");
    elements.codeInput.focus();
    return;
  }
  setBusy(true);
  showError("");
  hideTerminalState();
  try {
    const response = await fetch(
      `/api/device-authorization?user_code=${encodeURIComponent(userCode)}`,
      { headers: { Accept: "application/json" } },
    );
    const body = await readResponseBody(response);
    if (!response.ok) {
      throw new Error(body.error || "That device code could not be checked.");
    }
    currentAuthorization = body;
    elements.codeInput.value = formatCode(body.user_code);
    elements.codeDisplay.textContent = formatCode(body.user_code);
    elements.clientName.textContent = body.client.name;
    elements.scopeList.replaceChildren(
      ...body.scopes.map((scope) => {
        const item = document.createElement("li");
        const title = document.createElement("strong");
        const description = document.createElement("span");
        title.textContent = scope.name;
        description.textContent = scope.description;
        item.append(title, description);
        return item;
      }),
    );
    renderNextStep();
  } catch (error) {
    currentAuthorization = null;
    renderNextStep();
    showError(friendlyError(error));
  } finally {
    setBusy(false);
  }
}

function renderNextStep() {
  const view = deviceView({
    hasAuthorization: Boolean(currentAuthorization),
    isSignedIn: Boolean(currentUser),
    isComplete: !elements.complete.hidden,
  });
  elements.codeForm.hidden = !view.showCodeForm;
  elements.request.hidden = !view.showRequest;
  elements.signIn.hidden = !view.showSignIn;
  elements.consent.hidden = !view.showConsent;
  elements.complete.hidden = !view.showComplete;
  if (currentUser) {
    elements.accountName.textContent =
      currentUser.displayName || currentUser.email || "your Sneat Co. account";
  }
}

async function signInWithProvider(providerName) {
  setBusy(true);
  showError("");
  try {
    const provider =
      providerName === "github" ? new GithubAuthProvider() : new GoogleAuthProvider();
    await signInWithPopup(auth, provider);
  } catch (error) {
    showError(friendlyError(error));
  } finally {
    setBusy(false);
  }
}

async function signInWithEmail(event) {
  event.preventDefault();
  const values = new FormData(elements.emailForm);
  const email = String(values.get("email") || "").trim();
  const password = String(values.get("password") || "");
  setBusy(true);
  showError("");
  try {
    if (emailMode === "signup") {
      await createUserWithEmailAndPassword(auth, email, password);
    } else {
      await signInWithEmailAndPassword(auth, email, password);
    }
  } catch (error) {
    showError(friendlyError(error));
  } finally {
    setBusy(false);
  }
}

function toggleEmailMode() {
  emailMode = emailMode === "signin" ? "signup" : "signin";
  const password = elements.emailForm.elements.password;
  if (emailMode === "signup") {
    elements.emailSubmit.textContent = "Create Sneat Co. account";
    elements.emailToggle.textContent = "Sign in to an existing account";
    password.autocomplete = "new-password";
  } else {
    elements.emailSubmit.textContent = "Sign in with email";
    elements.emailToggle.textContent = "Create a Sneat Co. account instead";
    password.autocomplete = "current-password";
  }
  showError("");
}

async function decide(decision) {
  if (!currentAuthorization || !currentUser || busy) return;
  setBusy(true);
  showError("");
  try {
    const idToken = await currentUser.getIdToken(true);
    const response = await fetch("/api/device-authorization/decision", {
      method: "POST",
      headers: {
        Accept: "application/json",
        Authorization: `Bearer ${idToken}`,
        "Content-Type": "application/json",
      },
      body: JSON.stringify({
        user_code: currentAuthorization.user_code,
        decision,
      }),
    });
    const body = await readResponseBody(response);
    if (!response.ok) {
      throw new Error(body.error || "The authorization could not be completed.");
    }
    elements.completeTitle.textContent =
      decision === "approve" ? "Device authorized" : "Request denied";
    elements.completeMessage.textContent =
      decision === "approve"
        ? "Return to your terminal. The OpenVaultDB CLI will finish signing in."
        : "No access was granted. You can close this page.";
    elements.complete.hidden = false;
    renderNextStep();
    elements.complete.focus();
  } catch (error) {
    showError(friendlyError(error));
  } finally {
    setBusy(false);
  }
}

function hideTerminalState() {
  elements.complete.hidden = true;
  renderNextStep();
}

function setBusy(value) {
  busy = value;
  document.querySelectorAll("button, input").forEach((element) => {
    element.disabled = value;
  });
  elements.codeSubmit.textContent = value ? "Checking…" : "Continue";
}

function showError(message) {
  elements.error.textContent = message;
  elements.error.hidden = !message;
}

function normalizeCode(value) {
  return String(value || "")
    .toUpperCase()
    .replace(/[^A-Z0-9]/gu, "")
    .slice(0, 8);
}

function formatCode(value) {
  const code = normalizeCode(value);
  return code.length > 4 ? `${code.slice(0, 4)}-${code.slice(4)}` : code;
}

function friendlyError(error) {
  const code = error && error.code ? String(error.code) : "";
  if (code.includes("popup-closed")) return "Sign-in was cancelled.";
  if (code.includes("invalid-credential") || code.includes("wrong-password")) {
    return "Incorrect email or password.";
  }
  if (code.includes("email-already-in-use")) {
    return "That email is already registered. Sign in instead.";
  }
  if (code.includes("weak-password")) return "Password should be at least six characters.";
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
