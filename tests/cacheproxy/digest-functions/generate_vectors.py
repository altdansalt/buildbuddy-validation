"""Regenerate independent BLAKE3 oracles: pip install blake3==1.0.11."""
import hashlib
import json
import pathlib
import struct

import blake3

root = pathlib.Path(__file__).parent
sizes = [16 << 20, 256 << 20]
large = []
for size in sizes:
    h = blake3.blake3()
    for offset in range(0, size, 1 << 20):
        h.update(bytes(i % 251 for i in range(offset, min(size, offset + (1 << 20)))))
    large.append({"size": size, "hash": h.hexdigest()})
random = []
for index, size in enumerate([1, 63, 64, 65, 1023, 1024, 1025, 2048,
                              4097, 65537, 131073, 262145]):
    data = b"".join(hashlib.sha256(struct.pack("<QQ", index, counter)).digest()
                    for counter in range((size + 31) // 32))[:size]
    random.append({"size": size, "hash": blake3.blake3(data).hexdigest()})
for name, vectors in [("large", large), ("random", random)]:
    (root / f"{name}_vectors.json").write_text(json.dumps(vectors, indent=2) + "\n")
