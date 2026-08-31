import { createRemoteJWKSet, jwtVerify, type JWTVerifyGetKey } from "jose";

export interface Identity {
  subject: string;
  email?: string;
  name?: string;
}

export type VerifyIdentity = (
  idToken: string,
  projectID: string,
) => Promise<Identity>;

const firebaseKeys = createRemoteJWKSet(
  new URL(
    "https://www.googleapis.com/service_accounts/v1/jwk/securetoken@system.gserviceaccount.com",
  ),
);

export const verifyFirebaseIdentity: VerifyIdentity = (idToken, projectID) =>
  verifyFirebaseIdentityWithKeySet(idToken, projectID, firebaseKeys);

export async function verifyFirebaseIdentityWithKeySet(
  idToken: string,
  projectID: string,
  keySet: JWTVerifyGetKey,
): Promise<Identity> {
  if (projectID === "") {
    throw new Error("Firebase project ID is required");
  }
  const { payload } = await jwtVerify(idToken, keySet, {
    algorithms: ["RS256"],
    audience: projectID,
    issuer: `https://securetoken.google.com/${projectID}`,
    maxTokenAge: "65m",
    clockTolerance: "5s",
  });
  if (
    typeof payload.sub !== "string" ||
    payload.sub === "" ||
    payload.sub.length > 128
  ) {
    throw new Error("Firebase ID token has an invalid subject");
  }
  const now = Math.floor(Date.now() / 1000);
  if (typeof payload.auth_time !== "number" || payload.auth_time > now + 5) {
    throw new Error("Firebase ID token has an invalid authentication time");
  }
  return {
    subject: payload.sub,
    email: typeof payload.email === "string" ? payload.email : undefined,
    name: typeof payload.name === "string" ? payload.name : undefined,
  };
}
