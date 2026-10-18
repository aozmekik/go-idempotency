#!/usr/bin/env python3
"""Independent implementation of the go-idempotency request fingerprint.

The encoding is specified in docs/adr/0001-fingerprint-and-lock-semantics.md.
TestFingerprintIsStable in fingerprint_test.go pins the Go implementation to
the digest this script prints, so the two can be checked against each other:

    python3 testdata/fingerprint.py

It uses only the Python standard library.
"""

import hashlib

VERSION = b"go-idempotency/fingerprint/v1"


def uvarint(n: int) -> bytes:
    """Encode n like Go's encoding/binary.AppendUvarint (LEB128)."""
    out = bytearray()
    while n >= 0x80:
        out.append((n & 0x7F) | 0x80)
        n >>= 7
    out.append(n)
    return bytes(out)


def field(b: bytes) -> bytes:
    return uvarint(len(b)) + b


def fingerprint(method, host, escaped_path, raw_query, headers, body):
    """headers: list of (canonical name, [values]) sorted by name."""
    buf = field(VERSION)
    buf += field(method.encode())
    buf += field(host.lower().encode())
    buf += field(escaped_path.encode())
    buf += field(raw_query.encode())
    buf += uvarint(len(headers))
    for name, values in headers:
        buf += field(name.encode())
        buf += uvarint(len(values))
        for v in values:
            buf += field(v.encode())
    body_sum = hashlib.sha256(body).digest()
    buf += uvarint(len(body_sum)) + body_sum
    return hashlib.sha256(buf).hexdigest()


if __name__ == "__main__":
    # The request of TestFingerprintIsStable: httptest.NewRequest sets the
    # host to example.com.
    print(
        fingerprint(
            "POST",
            "example.com",
            "/payments",
            "x=1",
            [("Content-Type", ["application/json"])],
            b'{"amount":100}',
        )
    )
