const USER_CODE_ALPHABET = "BCDFGHJKLMNPQRSTVWXZ";

export function randomID(): string {
  return crypto.randomUUID();
}

export function randomSecret(prefix = ""): string {
  const bytes = new Uint8Array(32);
  crypto.getRandomValues(bytes);
  return prefix + base64URL(bytes);
}

export function randomUserCode(): string {
	const characters: string[] = [];
	const unbiasedLimit =
		Math.floor(256 / USER_CODE_ALPHABET.length) * USER_CODE_ALPHABET.length;
	while (characters.length < 8) {
		const bytes = new Uint8Array(8);
		crypto.getRandomValues(bytes);
		for (const value of bytes) {
			if (value < unbiasedLimit) {
				characters.push(USER_CODE_ALPHABET[value % USER_CODE_ALPHABET.length]);
			}
			if (characters.length === 8) break;
		}
	}
	return `${characters.slice(0, 4).join("")}-${characters.slice(4).join("")}`;
}

export function normalizeUserCode(value: string): string {
  const compact = value.toUpperCase().replace(/[^A-Z0-9]/g, "");
  if (compact.length !== 8) {
    return "";
  }
  return `${compact.slice(0, 4)}-${compact.slice(4)}`;
}

export async function hashSecret(value: string): Promise<string> {
  const digest = await crypto.subtle.digest(
    "SHA-256",
    new TextEncoder().encode(value),
  );
  return base64URL(new Uint8Array(digest));
}

function base64URL(bytes: Uint8Array): string {
  let binary = "";
  for (const byte of bytes) {
    binary += String.fromCharCode(byte);
  }
  return btoa(binary)
    .replaceAll("+", "-")
    .replaceAll("/", "_")
    .replace(/=+$/u, "");
}
