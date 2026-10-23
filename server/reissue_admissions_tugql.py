#!/usr/bin/env python3
"""Propose /tugql reissues of the operator-owned ECB, ECB-public and IANA admissions.

The admission documents are operator-owned and digest-pinned (OVDB_*_ADMISSION_SHA256
is the SHA-256 of the exact file bytes), so this tool never edits a file in place and
never touches the environment. For each file given it writes `<name>.tugql.json` next
to it with only the pre-rename route segment changed in the `path`, `workerPath` and
`goPath` fields, and prints the new digest and the environment variable to update.
The operator reviews, approves and deploys the reissued file; approval metadata
(`approvedBy`, `approvedAt`, expiry) is deliberately left as it was.

  python3 reissue_admissions_tugql.py --ecb ecb-admission.json \
      --ecb-public ecb-public-admission.json --iana iana-admission.json
"""
import argparse
import hashlib
import re
import sys
from pathlib import Path

OLD = "dtql"  # the pre-rename route segment named by existing admissions
NEW = "tugql"
KINDS = {
    "ecb": ("OVDB_ECB_ADMISSION_FILE", "OVDB_ECB_ADMISSION_SHA256", ("path",)),
    "ecb-public": ("OVDB_ECB_PUBLIC_ADMISSION_FILE", "OVDB_ECB_PUBLIC_ADMISSION_SHA256", ("workerPath", "goPath")),
    "iana": ("OVDB_IANA_ADMISSION_FILE", "OVDB_IANA_ADMISSION_SHA256", ("path",)),
}


def reissue(raw, fields):
    """Return (new bytes, number of replaced fields); the file's other bytes are untouched."""
    text = raw.decode("utf-8")
    count = 0
    for field in fields:
        pattern = re.compile(r'("%s"\s*:\s*"[^"]*/)%s(")' % (re.escape(field), OLD))
        text, replaced = pattern.subn(lambda m: m.group(1) + NEW + m.group(2), text)
        count += replaced
    return text.encode("utf-8"), count


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    for kind in KINDS:
        parser.add_argument("--" + kind, type=Path)
    args = parser.parse_args(argv)
    given = [(kind, getattr(args, kind.replace("-", "_"))) for kind in KINDS if getattr(args, kind.replace("-", "_"))]
    if not given:
        parser.error("give at least one admission file")
    status = 0
    for kind, path in given:
        file_env, digest_env, fields = KINDS[kind]
        raw = path.read_bytes()
        new, count = reissue(raw, fields)
        if count != len(fields):
            print("%s: %s names %d of %d expected %s route fields; nothing written" % (kind, path, count, len(fields), OLD), file=sys.stderr)
            status = 1
            continue
        target = path.with_name(path.stem + ".tugql" + path.suffix)
        target.write_bytes(new)
        print("%s: wrote %s\n  %s=%s\n  %s=%s" % (kind, target, file_env, target, digest_env, hashlib.sha256(new).hexdigest()))
    return status


if __name__ == "__main__":
    sys.exit(main())
