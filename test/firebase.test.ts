import {
  SignJWT,
  createLocalJWKSet,
  exportJWK,
  generateKeyPair,
} from "jose";
import { beforeAll, describe, expect, it } from "vitest";

import { verifyFirebaseIdentityWithKeySet } from "../src/firebase";

const projectID = "sneat-eur3-1";
let privateKey: CryptoKey;
let keySet: ReturnType<typeof createLocalJWKSet>;

beforeAll(async () => {
  const keyPair = await generateKeyPair("RS256", { extractable: true });
  privateKey = keyPair.privateKey as CryptoKey;
  const publicJWK = await exportJWK(keyPair.publicKey);
  publicJWK.alg = "RS256";
  publicJWK.kid = "test-key";
  publicJWK.use = "sig";
  keySet = createLocalJWKSet({ keys: [publicJWK] });
});

describe("Firebase ID token verification", () => {
  it("accepts a current signed token for the configured project", async () => {
    const now = Math.floor(Date.now() / 1000);
    const token = await signedToken({
      auth_time: now - 10,
      email: "alex@example.com",
      name: "Alex",
    });

    await expect(
      verifyFirebaseIdentityWithKeySet(token, projectID, keySet),
    ).resolves.toEqual({
      subject: "sneat-user-1",
      email: "alex@example.com",
      name: "Alex",
    });
  });

  it("rejects the wrong audience and a future authentication time", async () => {
    const now = Math.floor(Date.now() / 1000);
    const independentProjectToken = await signedToken(
      { auth_time: now - 10 },
      {
        audience: "openvaultdb",
        issuer: "https://securetoken.google.com/openvaultdb",
      },
    );
    await expect(
      verifyFirebaseIdentityWithKeySet(independentProjectToken, projectID, keySet),
    ).rejects.toThrow();

    const futureAuthentication = await signedToken({ auth_time: now + 60 });
    await expect(
      verifyFirebaseIdentityWithKeySet(futureAuthentication, projectID, keySet),
    ).rejects.toThrow("authentication time");
  });
});

async function signedToken(
  claims: Record<string, unknown>,
  options: { audience?: string; issuer?: string; subject?: string } = {},
): Promise<string> {
  const now = Math.floor(Date.now() / 1000);
  return new SignJWT(claims)
    .setProtectedHeader({ alg: "RS256", kid: "test-key" })
    .setIssuer(options.issuer ?? `https://securetoken.google.com/${projectID}`)
    .setAudience(options.audience ?? projectID)
    .setSubject(options.subject ?? "sneat-user-1")
    .setIssuedAt(now)
    .setExpirationTime(now + 3600)
    .sign(privateKey);
}
