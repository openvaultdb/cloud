/**
 * Parses JSONC (wrangler.jsonc): strips // and /* *\/ comments outside strings and trailing commas.
 * JSON.parse alone rejects the comments the Worker config carries.
 */
export function parseJSONC(text) {
  // Pass 1: drop comments. Pass 2: drop trailing commas. Both passes track string
  // literals, so text inside a string (including ",}" or "//") is never altered.
  let noComments = "";
  let inString = false;
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if (inString) {
      noComments += c;
      if (c === "\\") noComments += text[++i] ?? "";
      else if (c === '"') inString = false;
    } else if (c === '"') {
      inString = true;
      noComments += c;
    } else if (c === "/" && text[i + 1] === "/") {
      while (i < text.length && text[i] !== "\n") i++;
      noComments += "\n";
    } else if (c === "/" && text[i + 1] === "*") {
      const end = text.indexOf("*/", i + 2);
      if (end < 0) throw new Error("unterminated comment");
      i = end + 1;
      noComments += " ";
    } else {
      noComments += c;
    }
  }
  let out = "";
  inString = false;
  for (let i = 0; i < noComments.length; i++) {
    const c = noComments[i];
    if (inString) {
      out += c;
      if (c === "\\") out += noComments[++i] ?? "";
      else if (c === '"') inString = false;
    } else if (c === '"') {
      inString = true;
      out += c;
    } else if (c === ",") {
      let j = i + 1;
      while (j < noComments.length && /\s/u.test(noComments[j])) j++;
      if (noComments[j] !== "}" && noComments[j] !== "]") out += c;
    } else {
      out += c;
    }
  }
  return JSON.parse(out);
}
