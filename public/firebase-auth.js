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

const firebaseConfig = {
  apiKey: "AIzaSyCeQu1WC182yD0VHrRm4nHUxVf27fY-MLQ",
  authDomain: "auth.sneat.co",
  projectId: "sneat-eur3-1",
  messagingSenderId: "588648831063",
  appId: "1:588648831063:web:303af7e0c5f8a7b10d6b12",
};

export const auth = getAuth(initializeApp(firebaseConfig));
await setPersistence(auth, browserLocalPersistence).catch(() => undefined);

export { onAuthStateChanged, signOut };

export function accountName(user) {
  return user?.displayName || user?.email || "your Sneat Co. account";
}

export async function authenticateWithProvider(providerName) {
  const provider =
    providerName === "github" ? new GithubAuthProvider() : new GoogleAuthProvider();
  await signInWithPopup(auth, provider);
}

export async function authenticateWithEmail(email, password, createAccount = false) {
  if (createAccount) {
    await createUserWithEmailAndPassword(auth, email, password);
    return;
  }
  await signInWithEmailAndPassword(auth, email, password);
}
