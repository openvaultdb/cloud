import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

import { parseJSONC } from "./jsonc.mjs";

test("the real wrangler.jsonc parses as JSONC and binds no D1 database", async () => {
  const real = parseJSONC(await readFile(new URL("../wrangler.jsonc", import.meta.url), "utf8"));
  assert.equal(real.name, "openvaultdb-cloud");
  assert.equal(Object.hasOwn(real, "d1_databases"), false);
});

test("parseJSONC keeps comment-like text inside strings and tolerates trailing commas", () => {
  assert.deepEqual(parseJSONC('{"a": "http://x//y /* z */", /* c */ "b": [1,2,], // t\n}'), { a: "http://x//y /* z */", b: [1, 2] });
  assert.throws(() => parseJSONC("{ /* open"));
});

test("parseJSONC never alters string contents (comma-bracket, comment markers, escapes)", () => {
  assert.deepEqual(parseJSONC('{"a":"x,}y","b":"z,]"}'), { a: "x,}y", b: "z,]" });
  assert.deepEqual(parseJSONC('{"a":"x, }"}'), { a: "x, }" });
  assert.deepEqual(parseJSONC('{"a":"q\\",}","b":"//c","c":"/*d*/"}'), { a: 'q",}', b: "//c", c: "/*d*/" });
  assert.deepEqual(parseJSONC('{"a":[1,2, /* c */ ], "b":{"k":1, // t\n},}'), { a: [1, 2], b: { k: 1 } });
  assert.deepEqual(parseJSONC('{"a":1 /* block */, "b":2}'), { a: 1, b: 2 });
  assert.deepEqual(parseJSONC('{"a":[1,/* x */2]}'), { a: [1, 2] });
  assert.throws(() => parseJSONC('[1/* c */2]'), "a block comment separates tokens");
  assert.throws(() => parseJSONC('{"a":1,,}'));
  assert.throws(() => parseJSONC('{"a":[,1]}'));
});

test("parseJSONC matches JSON.parse on generated strict JSON, decorated with comments and trailing commas", () => {
  let seed = 12345;
  const rand = (n) => (seed = (seed * 1103515245 + 12345) & 0x7fffffff) % n;
  const tricky = ["x,}y", "z,]", "a, }", "//c", "/*d*/", 'q",}', "\\", "plain", ", ]", ""];
  const gen = (depth) => {
    const kind = depth > 3 ? rand(3) : rand(5);
    if (kind === 0) return tricky[rand(tricky.length)];
    if (kind === 1) return rand(1000);
    if (kind === 2) return rand(2) === 0;
    if (kind === 3) return Array.from({ length: rand(4) }, () => gen(depth + 1));
    return Object.fromEntries(Array.from({ length: rand(4) }, (_, i) => [`k${i}${tricky[rand(tricky.length)]}`, gen(depth + 1)]));
  };
  // Re-render strict JSON, adding a trailing comma and a comment before each closer.
  const decorate = (value) => {
    if (Array.isArray(value)) return `[${value.map(decorate).join(" /* c */ ,\n")}${value.length ? ", // t\n" : ""}]`;
    if (value && typeof value === "object") {
      const entries = Object.entries(value).map(([k, v]) => `${JSON.stringify(k)}: ${decorate(v)}`);
      return `{${entries.join(", /* c */\n")}${entries.length ? ", /* end */" : ""}}`;
    }
    return JSON.stringify(value);
  };
  for (let n = 0; n < 300; n++) {
    const value = gen(0);
    assert.deepEqual(parseJSONC(JSON.stringify(value)), JSON.parse(JSON.stringify(value)));
    assert.deepEqual(parseJSONC(decorate(value)), JSON.parse(JSON.stringify(value)));
  }
});
